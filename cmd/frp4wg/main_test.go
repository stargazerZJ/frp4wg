package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// readFromUDP reads one datagram with deadline and returns n, src, data.
func readFromUDP(t *testing.T, c *net.UDPConn, d time.Duration) (int, *net.UDPAddr, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 65535)
	n, src, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read timeout or error: %v", err)
	}
	return n, src, buf[:n]
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func strconvIt(i int) string {
	return strconv.Itoa(i)
}

// ---- Client-side test: first packet is forwarded before promotion, then bridge via single socket

func TestClient_ForwardFirstPacketAndBridgeSingleSocket(t *testing.T) {
	// Start fake "server" and "local WireGuard" UDP listeners on loopback.
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	defer serverConn.Close()
	wgConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("wg listen: %v", err)
	}
	defer wgConn.Close()

	serverAddr := serverConn.LocalAddr().(*net.UDPAddr)
	wgAddr := wgConn.LocalAddr().(*net.UDPAddr)

	// Start client with 1 standby and short intervals, using our loopback listeners.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := clientConfig{
		Server:            net.JoinHostPort(serverAddr.IP.String(), strconvIt(serverAddr.Port)),
		LocalWG:           net.JoinHostPort(wgAddr.IP.String(), strconvIt(wgAddr.Port)),
		StandbyN:          1,
		HandshakeInterval: 100 * time.Millisecond,
		HandshakeTries:    10,
		ActiveIdle:        5 * time.Second,
		ReadBufferSize:    65535,
		Logger:            newLogger(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runClientWithContext(ctx, cfg)
	}()

	// 1) Server receives initial handshake from client's standby
	_, clientFrom1, data := readFromUDP(t, serverConn, 2*time.Second)
	if string(data) != handshakeMagic {
		t.Fatalf("expected handshake from client, got: %x", data)
	}

	// Reply handshake
	if _, err := serverConn.WriteToUDP(magicBytes, clientFrom1); err != nil {
		t.Fatalf("server handshake reply write: %v", err)
	}

	// 2) Simulate server sending a WireGuard handshake (non-handshake payload) to client standby
	wgInit := []byte{0x01, 0x02, 0x03, 0x04}
	if _, err := serverConn.WriteToUDP(wgInit, clientFrom1); err != nil {
		t.Fatalf("server send wgInit: %v", err)
	}

	// 3) Local WireGuard should receive that very first packet forwarded immediately (0-RTT)
	_, clientFromOnWG, firstData := readFromUDP(t, wgConn, 2*time.Second)
	if string(firstData) == handshakeMagic {
		t.Fatalf("unexpected handshake forwarded to local wg")
	}
	if !equalBytes(firstData, wgInit) {
		t.Fatalf("wg first data mismatch: got %x want %x", firstData, wgInit)
	}

	// 4) From server to local wg: after activation, send another packet; local wg should receive it
	fromServer2 := []byte("from-server-2")
	if _, err := serverConn.WriteToUDP(fromServer2, clientFrom1); err != nil {
		t.Fatalf("server send fromServer2: %v", err)
	}
	_, _, got2 := readFromUDP(t, wgConn, 2*time.Second)
	if !equalBytes(got2, fromServer2) {
		t.Fatalf("wg got2 mismatch: got %x want %x", got2, fromServer2)
	}

	// 5) From local wg to server: send a packet back to the client's ephemeral port observed at wg
	fromLocal := []byte("from-local")
	if _, err := wgConn.WriteToUDP(fromLocal, clientFromOnWG); err != nil {
		t.Fatalf("wg send fromLocal: %v", err)
	}
	// The server may concurrently receive client standby handshakes. Ignore those control packets.
	buf := make([]byte, 65535)
	deadline := time.Now().Add(2 * time.Second)
	var got3 []byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _, err := serverConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Now().After(deadline) {
					t.Fatalf("timeout waiting for from-local at server")
				}
				continue
			}
			t.Fatalf("server read error: %v", err)
		}
		d := append([]byte(nil), buf[:n]...)
		if string(d) == handshakeMagic {
			// Ignore control
			if time.Now().After(deadline) {
				t.Fatalf("only handshake seen at server within deadline")
			}
			continue
		}
		got3 = d
		break
	}
	if !equalBytes(got3, fromLocal) {
		t.Fatalf("server got3 mismatch: got %x want %x", got3, fromLocal)
	}

	// Cleanup
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("client did not exit in time")
	}
}

// ---- Server-side unit test: standby, activation, and bidirectional routing

func TestServer_StandbyActivation_RoutesBothWays(t *testing.T) {
	// Reserve a free UDP port for server
	tmp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := tmp.LocalAddr().(*net.UDPAddr).Port
	_ = tmp.Close()

	serverBind := fmt.Sprintf("127.0.0.1:%d", port)
	serverAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := serverConfig{
		Bind:           serverBind,
		StandbyTTL:     5 * time.Second,
		ActiveIdle:     10 * time.Second,
		MaxStandby:     64,
		GCInterval:     500 * time.Millisecond,
		ReadBufferSize: 65535,
		Logger:         newLogger(),
	}
	doneSrv := make(chan struct{})
	go func() {
		defer close(doneSrv)
		_ = runServerWithContext(ctx, cfg)
	}()

	// Client socket that will become the "client" side of the mapping.
	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer clientConn.Close()

	// 1) Client sends handshake to server, receives handshake reply (retry until server is ready)
	handshakeDeadline := time.Now().Add(2 * time.Second)
	var from *net.UDPAddr
	var data []byte
	for {
		if _, err := clientConn.WriteToUDP(magicBytes, serverAddr); err != nil {
			t.Fatalf("client send handshake: %v", err)
		}
		_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 65535)
		n, f, err := clientConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Now().After(handshakeDeadline) {
					t.Fatalf("timeout waiting for handshake reply")
				}
				continue
			}
			t.Fatalf("client read handshake reply: %v", err)
		}
		from = f
		data = buf[:n]
		if string(data) == handshakeMagic {
			break
		}
		if time.Now().After(handshakeDeadline) {
			t.Fatalf("timeout without receiving proper handshake reply, got %x", data)
		}
	}
	if from.Port != serverAddr.Port || !from.IP.Equal(serverAddr.IP) {
		t.Fatalf("handshake reply from unexpected addr: %v", from)
	}

	// 2) Remote sends first packet to server; server should consume standby and forward to client
	remoteConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}
	defer remoteConn.Close()

	first := []byte("wg-init")
	if _, err := remoteConn.WriteToUDP(first, serverAddr); err != nil {
		t.Fatalf("remote send first: %v", err)
	}
	_, _, gotFirstAtClient := readFromUDP(t, clientConn, 2*time.Second)
	if !equalBytes(gotFirstAtClient, first) {
		t.Fatalf("client did not receive first forwarded packet, got %x want %x", gotFirstAtClient, first)
	}

	// 3) Client -> Server should be routed to remote
	fromClient := []byte("from-client")
	if _, err := clientConn.WriteToUDP(fromClient, serverAddr); err != nil {
		t.Fatalf("client send: %v", err)
	}
	_, _, gotAtRemote := readFromUDP(t, remoteConn, 2*time.Second)
	if !equalBytes(gotAtRemote, fromClient) {
		t.Fatalf("remote got mismatch: %x want %x", gotAtRemote, fromClient)
	}

	// Cleanup
	cancel()
	select {
	case <-doneSrv:
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not exit in time")
	}
}

// ---- E2E test with a few concurrent connections

func TestE2E_ConcurrentClients(t *testing.T) {
	// Reserve server port
	tmp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := tmp.LocalAddr().(*net.UDPAddr).Port
	_ = tmp.Close()
	serverBind := fmt.Sprintf("127.0.0.1:%d", port)
	serverAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}

	// Start server
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srvCfg := serverConfig{
		Bind:           serverBind,
		StandbyTTL:     10 * time.Second,
		ActiveIdle:     10 * time.Second,
		MaxStandby:     256,
		GCInterval:     1 * time.Second,
		ReadBufferSize: 65535,
		Logger:         newLogger(),
	}
	doneSrv := make(chan struct{})
	go func() {
		defer close(doneSrv)
		_ = runServerWithContext(ctx, srvCfg)
	}()

	type clientCtx struct {
		wgConn         *net.UDPConn // local wireguard listener
		wgAddr         *net.UDPAddr
		clientSockSeen *net.UDPAddr // ephemeral addr observed at wgConn (client's a.conn)
		cancel         context.CancelFunc
		done           chan struct{}
	}

	N := 3
	cc := make([]*clientCtx, 0, N)

	// Start N clients
	for i := 0; i < N; i++ {
		wgConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if err != nil {
			t.Fatalf("wg listen %d: %v", i, err)
		}
		wgAddr := wgConn.LocalAddr().(*net.UDPAddr)

		cctx, ccancel := context.WithCancel(ctx)
		cfg := clientConfig{
			Server:            serverBind,
			LocalWG:           net.JoinHostPort(wgAddr.IP.String(), strconvIt(wgAddr.Port)),
			StandbyN:          2,
			HandshakeInterval: 100 * time.Millisecond,
			HandshakeTries:    10,
			ActiveIdle:        10 * time.Second,
			ReadBufferSize:    65535,
			Logger:            newLogger(),
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = runClientWithContext(cctx, cfg)
		}()
		cc = append(cc, &clientCtx{wgConn: wgConn, wgAddr: wgAddr, cancel: ccancel, done: done})
	}

	// For each client, activate by sending from a unique remote
	type remoteCtx struct {
		conn *net.UDPConn
		addr *net.UDPAddr
	}
	remotes := make([]*remoteCtx, 0, N)
	for i := 0; i < N; i++ {
		rc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if err != nil {
			t.Fatalf("remote %d listen: %v", i, err)
		}
		remotes = append(remotes, &remoteCtx{conn: rc, addr: rc.LocalAddr().(*net.UDPAddr)})
	}

	// Discover mapping between each remote and whichever client the server pairs it with.
	// Each remote i sends a unique first payload: 0x99, i, 0x01. We read from all client wgConns
	// until each remote is matched to some client.
	remoteToClient := make([]int, N)
	for i := range remoteToClient {
		remoteToClient[i] = -1
	}
	// inverse mapping: client index -> remote index
	clientToRemote := make([]int, N)
	for i := range clientToRemote {
		clientToRemote[i] = -1
	}
	firstPkts := make([][]byte, N)
	for i := 0; i < N; i++ {
		firstPkts[i] = []byte{0x99, byte(i), 0x01}
	}
	discoveryDeadline := time.Now().Add(7 * time.Second)
	buf := make([]byte, 65535)
	assigned := 0
	for assigned < N {
		// Send one first packet from each unmapped remote
		for i := 0; i < N; i++ {
			if remoteToClient[i] == -1 {
				if _, err := remotes[i].conn.WriteToUDP(firstPkts[i], serverAddr); err != nil {
					t.Fatalf("remote %d send first: %v", i, err)
				}
			}
		}
		// Probe all client wgConns for short window
		progress := false
		for j := 0; j < N; j++ {
			_ = cc[j].wgConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			n, from, err := cc[j].wgConn.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				t.Fatalf("client %d wg read error: %v", j, err)
			}
			d := append([]byte(nil), buf[:n]...)
			// Identify remote index by payload signature
			if len(d) >= 3 && d[0] == 0x99 && d[2] == 0x01 {
				idx := int(d[1])
				if idx >= 0 && idx < N && remoteToClient[idx] == -1 {
					remoteToClient[idx] = j
					clientToRemote[j] = idx
					cc[j].clientSockSeen = from
					progress = true
					assigned++
				}
			}
		}
		if assigned >= N {
			break
		}
		if !progress && time.Now().After(discoveryDeadline) {
			t.Fatalf("mapping discovery timed out; assigned=%d/%d", assigned, N)
		}
	}

	// Now concurrently exercise both directions for all mappings
	var wg sync.WaitGroup
	wg.Add(2 * N)

	// remote -> wg
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			msg := []byte("hello-" + strconvIt(i))
			c := remoteToClient[i]
			if c < 0 || c >= N {
				t.Fatalf("invalid mapping for remote %d -> client %d", i, c)
			}
			if _, err := remotes[i].conn.WriteToUDP(msg, serverAddr); err != nil {
				t.Fatalf("remote %d write: %v", i, err)
			}
			// Read until expected msg (other traffic may interleave)
			deadline := time.Now().Add(2 * time.Second)
			for {
				_ = cc[c].wgConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				buf := make([]byte, 65535)
				n, _, err := cc[c].wgConn.ReadFromUDP(buf)
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						if time.Now().After(deadline) {
							t.Fatalf("client %d wg did not receive expected msg from remote %d", c, i)
						}
						continue
					}
					t.Fatalf("client %d wg read error: %v", c, err)
				}
				got := append([]byte(nil), buf[:n]...)
				if equalBytes(got, msg) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("client %d wg did not receive expected msg from remote %d", c, i)
				}
			}
		}()
	}

	// wg -> remote
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			msg := []byte("pong-" + strconvIt(i))
			r := clientToRemote[i]
			if r < 0 || r >= N {
				t.Fatalf("invalid mapping for client %d -> remote %d", i, r)
			}
			if cc[i].clientSockSeen == nil {
				t.Fatalf("client %d missing clientSockSeen", i)
			}
			if _, err := cc[i].wgConn.WriteToUDP(msg, cc[i].clientSockSeen); err != nil {
				t.Fatalf("client %d wg write: %v", i, err)
			}
			// Read until expected msg (interleaving possible)
			deadline := time.Now().Add(2 * time.Second)
			for {
				_ = remotes[r].conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				buf := make([]byte, 65535)
				n, _, err := remotes[r].conn.ReadFromUDP(buf)
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						if time.Now().After(deadline) {
							t.Fatalf("remote %d did not receive expected msg from client %d", r, i)
						}
						continue
					}
					t.Fatalf("remote %d read error: %v", r, err)
				}
				got := append([]byte(nil), buf[:n]...)
				if equalBytes(got, msg) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("remote %d did not receive expected msg from client %d", r, i)
				}
			}
		}()
	}

	wg.Wait()

	// Cleanup clients and server
	for i := 0; i < N; i++ {
		cc[i].cancel()
		select {
		case <-cc[i].done:
		case <-time.After(2 * time.Second):
			t.Fatalf("client %d did not exit in time", i)
		}
		cc[i].wgConn.Close()
	}

	cancel()
	select {
	case <-doneSrv:
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not exit in time")
	}

	// Close remotes
	for _, r := range remotes {
		_ = r.conn.Close()
	}
}
