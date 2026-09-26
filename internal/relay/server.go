package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"srv.solsynth.dev/sosys/blade/internal/logging"
)

// Stats are the relay's live counters. All fields are atomic: handlers on the
// accept path update them without coordination.
type Stats struct {
	Active     atomic.Int64
	Accepted   atomic.Int64
	Rejected   atomic.Int64
	DialErrors atomic.Int64
	SNIRejects atomic.Int64
	BytesUp    atomic.Int64
	BytesDown  atomic.Int64

	start time.Time
}

// StatsSnapshot is the plain-value form of Stats, safe to serialize.
type StatsSnapshot struct {
	Active     int64 `json:"active"`
	Accepted   int64 `json:"accepted"`
	Rejected   int64 `json:"rejected"`
	DialErrors int64 `json:"dialErrors"`
	SNIRejects int64 `json:"sniRejects"`
	BytesUp    int64 `json:"bytesUp"`
	BytesDown  int64 `json:"bytesDown"`
}

// NewStats returns counters with the uptime clock started.
func NewStats() *Stats {
	return &Stats{start: time.Now()}
}

// Snapshot reads every counter once.
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		Active:     s.Active.Load(),
		Accepted:   s.Accepted.Load(),
		Rejected:   s.Rejected.Load(),
		DialErrors: s.DialErrors.Load(),
		SNIRejects: s.SNIRejects.Load(),
		BytesUp:    s.BytesUp.Load(),
		BytesDown:  s.BytesDown.Load(),
	}
}

// UptimeSeconds reports whole seconds since NewStats.
func (s *Stats) UptimeSeconds() int64 {
	if s == nil || s.start.IsZero() {
		return 0
	}
	elapsed := time.Since(s.start)
	if elapsed < 0 {
		return 0
	}
	return int64(elapsed / time.Second)
}

// Server is the L4 relay: it accepts TCP connections, routes them by SNI, and
// copies bytes between client and origin without ever terminating TLS.
type Server struct {
	cfg    Config
	stats  *Stats
	routes map[string]string

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// NewServer builds the SNI->target routing table from the configuration.
func NewServer(cfg Config, stats *Stats) (*Server, error) {
	routes := make(map[string]string, len(cfg.Relay.Upstreams))
	for _, rule := range cfg.Relay.Upstreams {
		sni := NormalizeSNI(rule.SNI)
		target := strings.TrimSpace(rule.Target)
		if sni == "" || target == "" {
			return nil, fmt.Errorf("relay upstream rule requires both sni and target")
		}
		if _, duplicate := routes[sni]; duplicate {
			return nil, fmt.Errorf("relay upstream sni %q is configured twice", sni)
		}
		routes[sni] = target
	}
	if stats == nil {
		stats = NewStats()
	}
	return &Server{
		cfg:    cfg,
		stats:  stats,
		routes: routes,
		conns:  make(map[net.Conn]struct{}),
	}, nil
}

// Serve accepts connections on ln until Shutdown is called. Handlers run in
// their own goroutines; Shutdown waits for them.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.isClosed() {
				return nil
			}
			return err
		}
		if !s.track(conn) {
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// Shutdown stops accepting, closes live connections, and waits for the
// handlers to finish or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	first := !s.closed
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	var closeErr error
	if first {
		if ln != nil {
			closeErr = ln.Close()
		}
		for _, conn := range conns {
			_ = conn.Close()
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return closeErr
	}
	return nil
}

// track registers conn for shutdown, or reports false when the server is
// already shutting down (the caller then drops the connection).
func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) handle(client net.Conn) {
	defer func() {
		s.untrack(client)
		_ = client.Close()
		s.stats.Active.Add(-1)
	}()
	s.stats.Active.Add(1)
	s.stats.Accepted.Add(1)

	// Reject over-capacity connections before reading a single byte.
	if max := s.cfg.Relay.MaxConnections; max > 0 && s.stats.Active.Load() > max {
		s.stats.Rejected.Add(1)
		logging.Log.Debug().
			Str("remote", client.RemoteAddr().String()).
			Int64("maxConnections", max).
			Msg("Relay closed an over-capacity connection")
		return
	}

	prefix, sni, err := ReadClientHello(client, s.cfg.Relay.MaxClientHelloBytes, s.cfg.Relay.SNITimeout)
	if err != nil {
		s.stats.SNIRejects.Add(1)
		logging.Log.Debug().
			Err(err).
			Str("remote", client.RemoteAddr().String()).
			Msg("Relay closed a connection without a usable client hello")
		return
	}

	target, ok := s.route(sni)
	if !ok {
		s.stats.SNIRejects.Add(1)
		logging.Log.Info().
			Str("sni", sni).
			Str("remote", client.RemoteAddr().String()).
			Msg("Relay has no upstream for the requested SNI")
		return
	}

	dialer := net.Dialer{Timeout: s.cfg.Relay.DialTimeout}
	upstream, err := dialer.Dial("tcp", target)
	if err != nil {
		s.stats.DialErrors.Add(1)
		logging.Log.Warn().
			Err(err).
			Str("sni", sni).
			Str("target", target).
			Msg("Relay could not dial the upstream")
		return
	}
	defer upstream.Close()

	// Replay the peeked bytes verbatim: the origin sees the untouched
	// ClientHello, and TLS continues end-to-end between client and origin.
	if _, err := upstream.Write(prefix); err != nil {
		logging.Log.Debug().
			Err(err).
			Str("sni", sni).
			Str("target", target).
			Msg("Relay could not replay the client hello upstream")
		return
	}

	// The client hello already reached the origin, so the pump copies what
	// follows it in both directions.
	s.pipe(client, upstream)
}

// route resolves an SNI to an upstream target, falling back to the configured
// default and reporting false when the relay has no upstream for it.
func (s *Server) route(sni string) (string, bool) {
	if target, ok := s.routes[NormalizeSNI(sni)]; ok {
		return target, true
	}
	if s.cfg.Relay.DefaultUpstream != "" {
		return s.cfg.Relay.DefaultUpstream, true
	}
	return "", false
}

// pipe copies in both directions until each direction ends, half-closing each
// destination so the peer observes EOF, then closes both connections.
func (s *Server) pipe(client, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyDirection(upstream, client, &s.stats.BytesUp)
	}()
	go func() {
		defer wg.Done()
		s.copyDirection(client, upstream, &s.stats.BytesDown)
	}()
	wg.Wait()
}

// copyDirection copies src into dst, counting the bytes relayed as they flow,
// and half-closes dst when the source reaches EOF.
func (s *Server) copyDirection(dst, src net.Conn, counter *atomic.Int64) {
	reader := &streamReader{conn: src, src: src, idle: s.cfg.Relay.IdleTimeout, counter: counter}
	if _, err := io.Copy(dst, reader); err != nil {
		logging.Log.Debug().
			Err(err).
			Str("remote", dst.RemoteAddr().String()).
			Msg("Relay copy ended with an error")
	}
	closeWrite(dst)
}

// streamReader counts the bytes relayed from one peer and, when an idle timeout
// is configured, refreshes the read deadline before every read so a silent peer
// cannot pin a connection open.
type streamReader struct {
	conn    net.Conn
	src     io.Reader
	idle    time.Duration
	counter *atomic.Int64
}

func (r *streamReader) Read(p []byte) (int, error) {
	if r.idle > 0 {
		if err := r.conn.SetReadDeadline(time.Now().Add(r.idle)); err != nil {
			return 0, err
		}
	}
	n, err := r.src.Read(p)
	if n > 0 {
		r.counter.Add(int64(n))
	}
	return n, err
}

// CloseWrite half-closes a TCP connection when the concrete type supports it.
func closeWrite(conn net.Conn) {
	if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	}
}
