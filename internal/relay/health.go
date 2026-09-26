package relay

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// NewHealthHandler serves the relay's own status surface:
//
//	GET /health  dials every configured upstream; 200 "ok" or 503 with failures
//	GET /status  the live connection and byte counters
//
// Any other path is a 404, including the /api/** paths Blade's proxy rewrite
// produces when it proxies the "relay" service.
func NewHealthHandler(cfg Config, stats *Stats) http.Handler {
	targets := upstreamTargets(cfg)
	dialTimeout := cfg.Relay.DialTimeout

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeHealth(w, targets, dialTimeout)
		case "/status":
			writeStatus(w, stats)
		default:
			http.NotFound(w, r)
		}
	})
}

// upstreamTargets lists every distinct dialed target: allowlist rules plus the
// default upstream.
func upstreamTargets(cfg Config) []string {
	seen := make(map[string]struct{}, len(cfg.Relay.Upstreams)+1)
	targets := make([]string, 0, len(cfg.Relay.Upstreams)+1)
	add := func(target string) {
		if target == "" {
			return
		}
		if _, duplicate := seen[target]; duplicate {
			return
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	for _, rule := range cfg.Relay.Upstreams {
		add(rule.Target)
	}
	add(cfg.Relay.DefaultUpstream)
	return targets
}

type unhealthyReport struct {
	Status   string            `json:"status"`
	Failures map[string]string `json:"failures"`
}

func writeHealth(w http.ResponseWriter, targets []string, dialTimeout time.Duration) {
	failures := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			dialer := net.Dialer{Timeout: dialTimeout}
			conn, err := dialer.Dial("tcp", target)
			if err != nil {
				mu.Lock()
				failures[target] = err.Error()
				mu.Unlock()
				return
			}
			_ = conn.Close()
		}(target)
	}
	wg.Wait()

	if len(failures) == 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(unhealthyReport{Status: "unhealthy", Failures: failures})
}

type statusReport struct {
	StatsSnapshot
	UptimeSeconds int64 `json:"uptimeSeconds"`
}

func writeStatus(w http.ResponseWriter, stats *Stats) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statusReport{
		StatsSnapshot: stats.Snapshot(),
		UptimeSeconds: stats.UptimeSeconds(),
	})
}
