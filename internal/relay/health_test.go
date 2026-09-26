package relay

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthHandlerReportsUpstreamReachability(t *testing.T) {
	// A listening socket with no Accept still completes a dial.
	reachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = reachable.Close() })

	cfg := testRelayConfig(reachable.Addr().String())
	cfg.Relay.DialTimeout = time.Second
	handler := NewHealthHandler(cfg, NewStats())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "ok" {
		t.Fatalf("/health = %d %q, want 200 ok", recorder.Code, recorder.Body.String())
	}
}

func TestHealthHandlerReportsUnreachableUpstream(t *testing.T) {
	cfg := testRelayConfig("127.0.0.1:1")
	cfg.Relay.DialTimeout = time.Second
	handler := NewHealthHandler(cfg, NewStats())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health = %d, want 503", recorder.Code)
	}
	var report unhealthyReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}
	if report.Status != "unhealthy" || report.Failures["127.0.0.1:1"] == "" {
		t.Fatalf("/health body = %+v, want the failed target reported", report)
	}
}

func TestHealthHandlerOnlyServesItsOwnPaths(t *testing.T) {
	cfg := testRelayConfig("127.0.0.1:1")
	handler := NewHealthHandler(cfg, NewStats())

	for _, path := range []string{"/", "/api/health", "/api/status", "/status/extra"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, recorder.Code)
		}
	}
}

func TestStatusHandlerReportsCounters(t *testing.T) {
	stats := NewStats()
	stats.Accepted.Add(3)
	stats.SNIRejects.Add(1)
	stats.BytesUp.Add(512)
	stats.BytesDown.Add(1024)

	recorder := httptest.NewRecorder()
	NewHealthHandler(testRelayConfig("127.0.0.1:1"), stats).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/status = %d, want 200", recorder.Code)
	}
	var report statusReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode /status body: %v", err)
	}
	if report.Accepted != 3 || report.SNIRejects != 1 || report.BytesUp != 512 || report.BytesDown != 1024 {
		t.Fatalf("/status = %+v, want the live counters", report)
	}
	if report.UptimeSeconds < 0 {
		t.Fatalf("uptimeSeconds = %d, want a non-negative uptime", report.UptimeSeconds)
	}
}
