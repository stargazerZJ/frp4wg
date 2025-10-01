package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	handshakeMagic = "frp4wg" // 6-byte sequence
)

var (
	magicBytes = []byte(handshakeMagic)
)

func main() {
	logger := newLogger()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	sub := strings.ToLower(os.Args[1])
	switch sub {
	case "c", "client":
		fs := flag.NewFlagSet("client", flag.ExitOnError)
		server := fs.String("server", "", "frps UDP address (host:port)")
		local := fs.String("local", "127.0.0.1:51820", "local WireGuard UDP address to forward to")
		standbyN := fs.Int("standby", 8, "number of standby connections to maintain")
		hsInterval := fs.Duration("handshake-interval", 5*time.Second, "interval between client handshakes")
		hsTries := fs.Int("handshake-tries", 3, "max consecutive missed handshake replies before recreating standby")
		activeIdle := fs.Duration("active-timeout", 2*time.Minute, "idle timeout for active connection")
		readBuf := fs.Int("read-buffer", 65535, "read buffer size in bytes")
		_ = fs.Parse(os.Args[2:])

		if *server == "" {
			slog.Error("missing -server")
			usage()
			os.Exit(2)
		}

		cfg := clientConfig{
			Server:            *server,
			LocalWG:           *local,
			StandbyN:          *standbyN,
			HandshakeInterval: *hsInterval,
			HandshakeTries:    *hsTries,
			ActiveIdle:        *activeIdle,
			ReadBufferSize:    *readBuf,
			Logger:            logger,
		}
		if err := runClient(cfg); err != nil {
			slog.Error("client exited with error", "err", err)
			os.Exit(1)
		}
	case "s", "server":
		fs := flag.NewFlagSet("server", flag.ExitOnError)
		bind := fs.String("bind", ":51820", "bind UDP address for frps (and WireGuard public port)")
		standbyTTL := fs.Duration("standby-ttl", 30*time.Second, "expire standby if no handshake within this duration")
		activeIdle := fs.Duration("active-timeout", 2*time.Minute, "idle timeout for active mapping")
		maxStandby := fs.Int("max-standby", 128, "max standby entries to keep (FIFO)")
		gcEvery := fs.Duration("gc-interval", 5*time.Second, "background GC check interval")
		readBuf := fs.Int("read-buffer", 65535, "read buffer size in bytes")
		_ = fs.Parse(os.Args[2:])

		cfg := serverConfig{
			Bind:             *bind,
			StandbyTTL:       *standbyTTL,
			ActiveIdle:       *activeIdle,
			MaxStandby:       *maxStandby,
			GCInterval:       *gcEvery,
			ReadBufferSize:   *readBuf,
			Logger:           logger,
		}
		if err := runServer(cfg); err != nil {
			slog.Error("server exited with error", "err", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		slog.Error("unknown subcommand", "sub", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  frp4wg c[lient] -server host:port [-local 127.0.0.1:51820] [-standby 8] [-handshake-interval 5s] [-handshake-tries 3] [-active-timeout 2m]\n")
	fmt.Fprintf(os.Stderr, "  frp4wg s[erver] -bind :51820 [-standby-ttl 30s] [-active-timeout 2m] [-max-standby 128]\n")
}

// ---------- logging

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v, ok := os.LookupEnv("FRP4WG_LOG_LEVEL"); ok {
		switch strings.ToLower(v) {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	})
	return slog.New(h)
}

// ---------- shared

func isHandshake(b []byte) bool {
	return bytes.Equal(b, magicBytes)
}

func addrKey(a *net.UDPAddr) string {
	if a == nil {
		return ""
	}
	return net.JoinHostPort(a.IP.String(), strconv.Itoa(a.Port))
}

// ---------- client implementation

type clientConfig struct {
	Server            string
	LocalWG           string
	StandbyN          int
	HandshakeInterval time.Duration
	HandshakeTries    int
	ActiveIdle        time.Duration
	ReadBufferSize    int
	Logger            *slog.Logger
}

type clientManager struct {
	cfg        clientConfig
	serverAddr *net.UDPAddr
	localAddr  *net.UDPAddr

	mu       sync.Mutex
	standbys map[string]*clientStandby // key=local UDP listen addr
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func runClient(cfg clientConfig) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return runClientWithContext(ctx, cfg)
}

// runClientWithContext is the same as runClient but accepts a parent context for tests.
func runClientWithContext(ctx context.Context, cfg clientConfig) error {
	logger := cfg.Logger
	serverAddr, err := net.ResolveUDPAddr("udp", cfg.Server)
	if err != nil {
		return fmt.Errorf("resolve server: %w", err)
	}
	localAddr, err := net.ResolveUDPAddr("udp", cfg.LocalWG)
	if err != nil {
		return fmt.Errorf("resolve local wg: %w", err)
	}

	m := &clientManager{
		cfg:        cfg,
		serverAddr: serverAddr,
		localAddr:  localAddr,
		standbys:   make(map[string]*clientStandby),
	}
	m.ctx, m.cancel = context.WithCancel(ctx)

	logger.Info("client start",
		"server", serverAddr.String(),
		"local", localAddr.String(),
		"standbyN", cfg.StandbyN,
		"handshakeInterval", cfg.HandshakeInterval,
		"activeIdle", cfg.ActiveIdle,
	)

	// Supervisor to maintain standby pool
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.ensureStandbys()
			}
		}
	}()

	// Wait for signal or parent cancel
	<-m.ctx.Done()
	logger.Info("client stopping...")
	m.shutdown()
	return nil
}

func (m *clientManager) ensureStandbys() {
	m.mu.Lock()
	defer m.mu.Unlock()
	needed := m.cfg.StandbyN - len(m.standbys)
	for i := 0; i < needed; i++ {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6zero, Port: 0})
		if err != nil {
			slog.Error("listen udp for standby", "err", err)
			return
		}
		if m.cfg.ReadBufferSize > 0 {
			_ = conn.SetReadBuffer(m.cfg.ReadBufferSize)
			_ = conn.SetWriteBuffer(m.cfg.ReadBufferSize)
		}
		key := addrKey(conn.LocalAddr().(*net.UDPAddr))
		sb := &clientStandby{
			m:      m,
			conn:   conn,
			key:    key,
			logger: m.cfg.Logger.With("standby", key),
		}
		m.standbys[key] = sb
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			sb.run()
		}()
	}
}

func (m *clientManager) promoteToActive(sb *clientStandby) {
	m.mu.Lock()
	// Remove from standbys map (it will close itself on return)
	delete(m.standbys, sb.key)
	m.mu.Unlock()

	act := &clientActive{
		m:           m,
		conn:        sb.conn, // reuse the same NAT mapping socket
		serverAddr:  m.serverAddr,
		localWGAddr: m.localAddr,
		logger:      m.cfg.Logger.With("active", sb.key),
		idle:        m.cfg.ActiveIdle,
		bufSize:     m.cfg.ReadBufferSize,
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		act.run()
	}()
}

func (m *clientManager) removeStandby(sb *clientStandby) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.standbys, sb.key)
}

func (m *clientManager) shutdown() {
	m.cancel()

	m.mu.Lock()
	for _, sb := range m.standbys {
		_ = sb.conn.Close()
	}
	m.standbys = map[string]*clientStandby{}
	m.mu.Unlock()

	m.wg.Wait()
}

type clientStandby struct {
	m        *clientManager
	conn     *net.UDPConn
	key      string
	logger   *slog.Logger
	promoted bool
}

func (s *clientStandby) run() {
	defer func() {
		if !s.promoted {
			_ = s.conn.Close()
		}
		s.m.removeStandby(s)
	}()

	logger := s.logger

	interval := s.m.cfg.HandshakeInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	hsTries := s.m.cfg.HandshakeTries
	if hsTries <= 0 {
		hsTries = 3
	}
	lives := hsTries

	buf := make([]byte, s.m.cfg.ReadBufferSize)
	if len(buf) == 0 {
		buf = make([]byte, 65535)
	}

	_, err := s.conn.WriteToUDP(magicBytes, s.m.serverAddr)
	lives -= 1
	_ = s.conn.SetReadDeadline(time.Now().Add(interval))
	if err != nil {
		logger.Warn("handshake write failed", "err", err)
		return
	}

	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				_, err := s.conn.WriteToUDP(magicBytes, s.m.serverAddr)
				lives -= 1
				_ = s.conn.SetReadDeadline(time.Now().Add(interval))
				if err != nil {
					logger.Warn("handshake write failed", "err", err)
					return
				}
				if lives < 0 {
					logger.Info("no handshake reply after ", hsTries, "tries , recreate")
					return
				}
				continue
			}
			logger.Warn("standby read error", "err", err)
			return
		}

		if !from.IP.Equal(s.m.serverAddr.IP) || from.Port != s.m.serverAddr.Port {
			// ignore unexpected source
			continue
		}

		data := buf[:n]
		if isHandshake(data) {
			logger.Debug("handshake reply ok")
			lives += 1
			continue
		}

		// Received non-handshake from server: forward it to local WG immediately, then promote (0-RTT)
		if _, werr := s.conn.WriteToUDP(data, s.m.localAddr); werr != nil {
			logger.Warn("forward first packet to local wg failed", "err", werr)
			// Still promote so subsequent packets can be relayed
		} else {
			logger.Debug("forwarded first packet to local wg")
		}
		logger.Info("activation signal received, promoting to active")
		// Important: do not close s.conn; promote uses the same socket
		s.promoted = true
		s.m.promoteToActive(s)
		return
	}
}

type clientActive struct {
	m           *clientManager
	conn        *net.UDPConn  // socket toward server (keeps NAT mapping)
	serverAddr  *net.UDPAddr  // server endpoint
	localWGAddr *net.UDPAddr  // local WireGuard endpoint
	logger      *slog.Logger
	idle        time.Duration
	bufSize     int
}

func (a *clientActive) run() {
	defer func() {
		_ = a.conn.Close()
	}()

	logger := a.logger.With("server", a.serverAddr.String(), "localWG", a.localWGAddr.String())
	logger.Info("active start")

	// Use the same UDP socket (a.conn) to forward both directions in a single goroutine.
	// This ensures the very first datagram (WireGuard handshake) was already forwarded
	// during promotion, and subsequent packets will flow bidirectionally through the same port.
	if a.bufSize > 0 {
		_ = a.conn.SetReadBuffer(a.bufSize)
		_ = a.conn.SetWriteBuffer(a.bufSize)
	}

	lastActive := time.Now()
	buf := make([]byte, max(2048, a.bufSize))

	for {
		_ = a.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				// idle/stop checks
				select {
				case <-a.m.ctx.Done():
					logger.Info("active stop")
					return
				default:
				}
				if a.idle > 0 && time.Since(lastActive) >= a.idle {
					logger.Info("active idle timeout")
					return
				}
				continue
			}
			logger.Warn("active read error", "err", err)
			return
		}

		data := buf[:n]
		// drop control handshake packets in active mode
		if isHandshake(data) {
			continue
		}
		// Direction: server -> local WireGuard
		if from.IP.Equal(a.serverAddr.IP) && from.Port == a.serverAddr.Port {
			if _, werr := a.conn.WriteToUDP(data, a.localWGAddr); werr != nil {
				logger.Warn("forward to local wg failed", "err", werr)
				return
			}
			lastActive = time.Now()
			continue
		}

		// Direction: local WireGuard -> server
		if from.IP.Equal(a.localWGAddr.IP) && from.Port == a.localWGAddr.Port {
			if _, werr := a.conn.WriteToUDP(data, a.serverAddr); werr != nil {
				logger.Warn("forward to server failed", "err", werr)
				return
			}
			lastActive = time.Now()
			continue
		}

		// Unknown source: ignore
	}
}

// ---------- server implementation

type serverConfig struct {
	Bind           string
	StandbyTTL     time.Duration
	ActiveIdle     time.Duration
	MaxStandby     int
	GCInterval     time.Duration
	ReadBufferSize int
	Logger         *slog.Logger
}

type activePair struct {
	client *net.UDPAddr
	remote *net.UDPAddr

	lastMu sync.Mutex
	last   time.Time
}

func (p *activePair) touch() {
	p.lastMu.Lock()
	p.last = time.Now()
	p.lastMu.Unlock()
}
func (p *activePair) idle(d time.Duration) bool {
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return time.Since(p.last) >= d
}

type standbyEntry struct {
	addr     *net.UDPAddr
	lastSeen time.Time
}

type serverState struct {
	cfg   serverConfig
	conn  *net.UDPConn
	ctx   context.Context
	cancel context.CancelFunc
	wg    sync.WaitGroup

	// standby management
	standbyMu sync.Mutex
	standbyQ  []*standbyEntry       // FIFO
	standbyIx map[string]*standbyEntry

	// active mappings
	activeMu        sync.RWMutex
	activeByClient  map[string]*activePair
	activeByRemote  map[string]*activePair
}

func runServer(cfg serverConfig) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return runServerWithContext(ctx, cfg)
}

// runServerWithContext is the same as runServer but accepts a parent context for tests.
func runServerWithContext(ctx context.Context, cfg serverConfig) error {
	bindAddr, err := net.ResolveUDPAddr("udp", cfg.Bind)
	if err != nil {
		return fmt.Errorf("resolve bind: %w", err)
	}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()
	if cfg.ReadBufferSize > 0 {
		_ = conn.SetReadBuffer(cfg.ReadBufferSize)
		_ = conn.SetWriteBuffer(cfg.ReadBufferSize)
	}

	s := &serverState{
		cfg:            cfg,
		conn:           conn,
		standbyQ:       make([]*standbyEntry, 0, max(16, cfg.MaxStandby)),
		standbyIx:      make(map[string]*standbyEntry),
		activeByClient: make(map[string]*activePair),
		activeByRemote: make(map[string]*activePair),
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	cfg.Logger.Info("server start", "bind", conn.LocalAddr().String())

	// GC worker
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(maxDuration(1*time.Second, cfg.GCInterval))
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				s.gc()
			}
		}
	}()

	// main loop
	buf := make([]byte, cfg.ReadBufferSize)
	if len(buf) == 0 {
		buf = make([]byte, 65535)
	}

	for {
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				select {
				case <-s.ctx.Done():
					cfg.Logger.Info("server stopping...")
					s.cancel()
					s.wg.Wait()
					return nil
				default:
				}
				continue
			}
			return fmt.Errorf("server read: %w", err)
		}
		data := buf[:n]

		// Handshake handling
		if isHandshake(data) {
			s.recordStandby(src)
			_, _ = conn.WriteToUDP(magicBytes, src) // reply
			continue
		}

		// Active routing
		if s.routeIfActive(src, data) {
			continue
		}

		// Unknown src: treat as new remote, try to consume a standby
		client := s.popStandby()
		if client == nil {
			// No standby available, drop
			continue
		}
		// Create active pair
		pair := &activePair{client: client, remote: src, last: time.Now()}
		s.activeMu.Lock()
		s.activeByClient[addrKey(client)] = pair
		s.activeByRemote[addrKey(src)] = pair
		s.activeMu.Unlock()

		// Forward the very first datagram immediately (0-RTT)
		_, _ = conn.WriteToUDP(data, client)
	}
}

func (s *serverState) routeIfActive(src *net.UDPAddr, data []byte) bool {
	key := addrKey(src)
	s.activeMu.RLock()
	pair, okC := s.activeByClient[key]
	if !okC {
		pair, okC = s.activeByRemote[key]
	}
	s.activeMu.RUnlock()
	if pair == nil {
		return false
	}
	// Determine direction
	var dst *net.UDPAddr
	if okC {
		// from client -> remote
		if addrKey(src) == addrKey(pair.client) {
			dst = pair.remote
		} else {
			dst = pair.client
		}
	} else {
		// map not found
		return false
	}
	_, _ = s.conn.WriteToUDP(data, dst)
	pair.touch()
	return true
}

func (s *serverState) recordStandby(a *net.UDPAddr) {
	key := addrKey(a)

	s.standbyMu.Lock()
	defer s.standbyMu.Unlock()

	if e, ok := s.standbyIx[key]; ok {
		e.lastSeen = time.Now()
		// move to back (refresh FIFO position)
		s.removeStandbyLocked(key)
		s.appendStandbyLocked(e)
		return
	}

	e := &standbyEntry{addr: cloneUDPAddr(a), lastSeen: time.Now()}
	// enforce max
	if s.cfg.MaxStandby > 0 && len(s.standbyQ) >= s.cfg.MaxStandby {
		old := s.standbyQ[0]
		delete(s.standbyIx, addrKey(old.addr))
		s.standbyQ = s.standbyQ[1:]
	}
	s.appendStandbyLocked(e)
}

func (s *serverState) popStandby() *net.UDPAddr {
	s.standbyMu.Lock()
	defer s.standbyMu.Unlock()
	if len(s.standbyQ) == 0 {
		return nil
	}
	e := s.standbyQ[0]
	s.standbyQ = s.standbyQ[1:]
	delete(s.standbyIx, addrKey(e.addr))
	return e.addr
}

func (s *serverState) removeStandbyLocked(key string) {
	if _, ok := s.standbyIx[key]; !ok {
		return
	}
	// find and remove from queue
	for i, e := range s.standbyQ {
		if addrKey(e.addr) == key {
			s.standbyQ = append(s.standbyQ[:i], s.standbyQ[i+1:]...)
			break
		}
	}
	delete(s.standbyIx, key)
}

func (s *serverState) appendStandbyLocked(e *standbyEntry) {
	key := addrKey(e.addr)
	s.standbyQ = append(s.standbyQ, e)
	s.standbyIx[key] = e
}

func (s *serverState) gc() {
	now := time.Now()
	// cleanup standby
	ttl := s.cfg.StandbyTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	func() {
		s.standbyMu.Lock()
		defer s.standbyMu.Unlock()
		newQ := s.standbyQ[:0]
		for _, e := range s.standbyQ {
			if now.Sub(e.lastSeen) >= ttl {
				delete(s.standbyIx, addrKey(e.addr))
				continue
			}
			newQ = append(newQ, e)
		}
		s.standbyQ = newQ
	}()

	// cleanup active
	idle := s.cfg.ActiveIdle
	if idle <= 0 {
		idle = 2 * time.Minute
	}
	var removeKeysC, removeKeysR []string
	s.activeMu.RLock()
	for ck, p := range s.activeByClient {
		if p.idle(idle) {
			removeKeysC = append(removeKeysC, ck)
			removeKeysR = append(removeKeysR, addrKey(p.remote))
		}
	}
	s.activeMu.RUnlock()
	if len(removeKeysC) > 0 {
		s.activeMu.Lock()
		for _, k := range removeKeysC {
			delete(s.activeByClient, k)
		}
		for _, k := range removeKeysR {
			delete(s.activeByRemote, k)
		}
		s.activeMu.Unlock()
	}
}

// ---------- helpers

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func cloneUDPAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	ip := make(net.IP, len(a.IP))
	copy(ip, a.IP)
	var zone string
	if a.Zone != "" {
		zone = a.Zone
	}
	return &net.UDPAddr{
		IP:   ip,
		Port: a.Port,
		Zone: zone,
	}
}