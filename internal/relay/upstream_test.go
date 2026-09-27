package relay

import (
	"net"
	"testing"
	"time"
)

func TestUpstreamFailuresReportsReachableTargets(t *testing.T) {
	// A listening socket with no Accept still completes a dial.
	reachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = reachable.Close() })

	cfg := testRelayConfig(reachable.Addr().String())
	if failures := UpstreamFailures(cfg, time.Second); len(failures) != 0 {
		t.Fatalf("UpstreamFailures() = %v, want no failures", failures)
	}
}

func TestUpstreamFailuresReportsUnreachableTarget(t *testing.T) {
	cfg := testRelayConfig("127.0.0.1:1")

	failures := UpstreamFailures(cfg, time.Second)
	if failures["127.0.0.1:1"] == "" {
		t.Fatalf("UpstreamFailures() = %v, want the refused target reported", failures)
	}
}

func TestUpstreamFailuresCoversTheDefaultUpstream(t *testing.T) {
	cfg := testRelayConfig("127.0.0.1:1")
	cfg.Relay.Upstreams = nil
	cfg.Relay.DefaultUpstream = "127.0.0.1:2"

	failures := UpstreamFailures(cfg, time.Second)
	if failures["127.0.0.1:2"] == "" {
		t.Fatalf("UpstreamFailures() = %v, want the default upstream checked", failures)
	}
}
