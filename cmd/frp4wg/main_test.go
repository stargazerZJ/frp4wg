package main

import (
	"context"
	"net"
	"strconv"
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
		Server:            net.JoinHostPort(serverAddr.IP.String(), func() string { return strconvIt(serverAddr.Port) }()),
		LocalWG:           net.JoinHostPort(wgAddr.IP.String(), func() string { return strconvIt(wgAddr.Port) }()),
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