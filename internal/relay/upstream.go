package relay

import (
	"net"
	"sync"
	"time"
)

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

// UpstreamFailures dials every configured upstream and returns the ones that
// refused, keyed by target. It is the relay's whole health check: the result
// travels to Blade as the heartbeat's healthy flag, so this file is the only
// place that decides whether a node is healthy.
func UpstreamFailures(cfg Config, dialTimeout time.Duration) map[string]string {
	failures := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, target := range upstreamTargets(cfg) {
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
	return failures
}
