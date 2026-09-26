package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testRelayConfig(upstreamTarget string) Config {
	return Config{
		Relay: RelayConfig{
			Listen:              "127.0.0.1:0",
			PublicHost:          "relay-test.local",
			PublicPort:          443,
			ID:                  "test-01",
			Weight:              1,
			DialTimeout:         2 * time.Second,
			SNITimeout:          2 * time.Second,
			MaxClientHelloBytes: 8192,
			Upstreams:           []UpstreamRule{{SNI: "api.solian.app", Target: upstreamTarget}},
		},
	}
}

// startRelay serves cfg on a loopback listener and returns its address.
func startRelay(t *testing.T, cfg Config, stats *Stats) (*Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := NewServer(cfg, stats)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("NewServer() error = %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return server, listener.Addr().String()
}

// dialRelay opens a TLS client connection through the relay (the handshake is
// performed end-to-end with the origin; the relay only copies bytes).
func dialRelay(t *testing.T, address, serverName string) *tls.Conn {
	t.Helper()
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("tls dial through relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServerRoutesBySNIAndPreservesBytes(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("origin saw path %q, want /ping", r.URL.Path)
		}
		_, _ = io.WriteString(w, "pong")
	}))
	t.Cleanup(origin.Close)

	stats := NewStats()
	_, address := startRelay(t, testRelayConfig(origin.Listener.Addr().String()), stats)

	conn := dialRelay(t, address, "api.solian.app")
	request, err := http.NewRequest("GET", "https://api.solian.app/ping", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Fatalf("response = %d %q, want 200 pong", response.StatusCode, body)
	}

	snapshot := stats.Snapshot()
	if snapshot.BytesUp <= 0 || snapshot.BytesDown <= 0 {
		t.Fatalf("byte counters = %+v, want both directions counted", snapshot)
	}
	if snapshot.SNIRejects != 0 || snapshot.DialErrors != 0 || snapshot.Rejected != 0 {
		t.Fatalf("counters = %+v, want a clean routed connection", snapshot)
	}
}

func TestServerRejectsUnknownSNI(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(origin.Close)

	stats := NewStats()
	_, address := startRelay(t, testRelayConfig(origin.Listener.Addr().String()), stats)

	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, &tls.Config{
		ServerName:         "unknown.example",
		InsecureSkipVerify: true,
	}); err == nil {
		_ = conn.Close()
		t.Fatal("handshake with an unlisted SNI must fail")
	}
	waitFor(t, func() bool { return stats.SNIRejects.Load() == 1 }, "an SNI rejection")
}

func TestServerDefaultUpstream(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "default-origin")
	}))
	t.Cleanup(origin.Close)

	cfg := testRelayConfig("")
	cfg.Relay.Upstreams = nil
	cfg.Relay.DefaultUpstream = origin.Listener.Addr().String()
	stats := NewStats()
	_, address := startRelay(t, cfg, stats)

	conn := dialRelay(t, address, "other.example")
	request, err := http.NewRequest("GET", "https://other.example/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200 through the default upstream", response.StatusCode)
	}
}

func TestServerMaxConnections(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(origin.Close)

	cfg := testRelayConfig(origin.Listener.Addr().String())
	cfg.Relay.MaxConnections = 1
	stats := NewStats()
	_, address := startRelay(t, cfg, stats)

	// The first connection completes its handshake and stays open, so the
	// relay counts it as active.
	held := dialRelay(t, address, "api.solian.app")

	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, &tls.Config{
		ServerName:         "api.solian.app",
		InsecureSkipVerify: true,
	}); err == nil {
		_ = conn.Close()
		t.Fatal("a connection above maxConnections must be dropped")
	}
	waitFor(t, func() bool { return stats.Rejected.Load() == 1 }, "an over-capacity rejection")

	// Closing the first connection frees the slot.
	_ = held.Close()
	waitFor(t, func() bool { return stats.Active.Load() == 0 }, "the active count to drain")
}

func TestServerIdleTimeout(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(origin.Close)

	cfg := testRelayConfig(origin.Listener.Addr().String())
	cfg.Relay.IdleTimeout = 300 * time.Millisecond
	_, address := startRelay(t, cfg, NewStats())

	conn := dialRelay(t, address, "api.solian.app")

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the relay must drop a connection that goes idle")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle connection lived for %v, want it dropped after the idle timeout", elapsed)
	}
}

func TestServerShutdownClosesLiveConnections(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(origin.Close)

	cfg := testRelayConfig(origin.Listener.Addr().String())
	stats := NewStats()
	server, address := startRelay(t, cfg, stats)
	conn := dialRelay(t, address, "api.solian.app")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("Shutdown must close live relayed connections")
	}
	if stats.Active.Load() != 0 {
		t.Fatalf("active = %d, want 0 after shutdown", stats.Active.Load())
	}
}

func TestServerStatsAndRouting(t *testing.T) {
	server, _ := startRelay(t, testRelayConfig("127.0.0.1:1"), NewStats())

	if target, ok := server.route("API.Solian.App."); !ok || target != "127.0.0.1:1" {
		t.Fatalf("route(canonical sni) = %q, %v; want the configured upstream", target, ok)
	}
	if target, ok := server.route("other.example"); ok {
		t.Fatalf("route(other.example) = %q, want no route", target)
	}

	cfg := testRelayConfig("127.0.0.1:1")
	cfg.Relay.DefaultUpstream = "api.solian.app:443"
	fallback, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if target, ok := fallback.route("other.example"); !ok || target != "api.solian.app:443" {
		t.Fatalf("route(default) = %q, %v; want the default upstream", target, ok)
	}
}

func TestNewServerRejectsBrokenRules(t *testing.T) {
	if _, err := NewServer(Config{Relay: RelayConfig{Upstreams: []UpstreamRule{{SNI: "", Target: "x:1"}}}}, nil); err == nil {
		t.Fatal("NewServer must reject a rule without an SNI")
	}
	if _, err := NewServer(Config{Relay: RelayConfig{Upstreams: []UpstreamRule{{SNI: "a.example", Target: ""}}}}, nil); err == nil {
		t.Fatal("NewServer must reject a rule without a target")
	}
	duplicate := testRelayConfig("x:1")
	duplicate.Relay.DefaultUpstream = "y:2"
	duplicate.Relay.Upstreams = append(duplicate.Relay.Upstreams, UpstreamRule{SNI: "API.solian.app", Target: "y:2"})
	if _, err := NewServer(duplicate, nil); err == nil {
		t.Fatal("NewServer must reject a duplicate SNI")
	}
}
