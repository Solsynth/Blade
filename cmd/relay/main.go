// Command relay runs a Blade L4 relay node: it terminates no TLS, it just
// routes connections by the SNI in the ClientHello and copies bytes to an
// origin, while announcing itself to Blade's service registry.
package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"srv.solsynth.dev/sosys/blade/internal/logging"
	"srv.solsynth.dev/sosys/blade/internal/relay"
)

const (
	shutdownTimeout  = 5 * time.Second
	statsLogInterval = time.Minute
)

func main() {
	pretty := os.Getenv("ZEROLOG_PRETTY") == "true"
	logging.Init(pretty)

	configPath := os.Getenv("RELAY_CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/relay.toml"
	}

	cfg, err := relay.Load(configPath)
	if err != nil {
		logging.Log.Fatal().Err(err).Str("configPath", configPath).Msg("Failed to load relay config")
	}

	upstreams := make(map[string]string, len(cfg.Relay.Upstreams))
	for _, rule := range cfg.Relay.Upstreams {
		upstreams[rule.SNI] = rule.Target
	}
	logging.Log.Info().
		Str("configPath", configPath).
		Str("id", cfg.Relay.ID).
		Str("listen", cfg.Relay.Listen).
		Str("publicAddress", cfg.Relay.PublicAddress()).
		Interface("upstreams", upstreams).
		Str("defaultUpstream", cfg.Relay.DefaultUpstream).
		Msg("Starting Blade relay node")

	listener, err := net.Listen("tcp", cfg.Relay.Listen)
	if err != nil {
		logging.Log.Fatal().Err(err).Str("listen", cfg.Relay.Listen).Msg("Failed to listen for relay connections")
	}

	stats := relay.NewStats()
	server, err := relay.NewServer(*cfg, stats)
	if err != nil {
		logging.Log.Fatal().Err(err).Msg("Failed to build relay server")
	}

	go func() {
		logging.Log.Info().Str("listen", cfg.Relay.Listen).Msg("Relay is accepting connections")
		if err := server.Serve(listener); err != nil {
			logging.Log.Fatal().Err(err).Msg("Relay server stopped")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go logCounters(ctx, stats)

	var registration *relay.Registration
	if cfg.Discovery.Enabled {
		registration, err = relay.NewRegistration(*cfg, func(ctx context.Context) map[string]string {
			return relay.UpstreamFailures(*cfg, cfg.Relay.DialTimeout)
		})
		if err != nil {
			logging.Log.Fatal().Err(err).Str("url", cfg.Discovery.URL).Msg("Failed to build the discovery client")
		}
		go registration.Run(ctx)
		logging.Log.Info().
			Str("url", cfg.Discovery.URL).
			Str("instance", cfg.Relay.ID).
			Msg("Announcing this relay to the Blade catalog over the public entry")
	} else {
		logging.Log.Warn().Msg("Relay discovery is disabled; this relay will not be listed by Blade")
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logging.Log.Info().Msg("Shutting down relay...")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	// Stop renewing first, then stop accepting and drop live connections, then
	// withdraw the registration so clients stop discovering this node.
	cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logging.Log.Warn().Err(err).Msg("Relay server shutdown was incomplete")
	}
	if registration != nil {
		registration.Deregister(shutdownCtx)
	}

	writeCounters(stats)
	logging.Log.Info().Msg("Relay exited")
}

// logCounters writes one counter snapshot now and then one per statsLogInterval
// until ctx ends. The node has no status listener, so this log line is the only
// view of live relayed traffic.
func logCounters(ctx context.Context, stats *relay.Stats) {
	writeCounters(stats)
	ticker := time.NewTicker(statsLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeCounters(stats)
		}
	}
}

// writeCounters emits the current counters as one log line.
func writeCounters(stats *relay.Stats) {
	snapshot := stats.Snapshot()
	logging.Log.Info().
		Int64("active", snapshot.Active).
		Int64("accepted", snapshot.Accepted).
		Int64("rejected", snapshot.Rejected).
		Int64("sniRejects", snapshot.SNIRejects).
		Int64("dialErrors", snapshot.DialErrors).
		Int64("bytesUp", snapshot.BytesUp).
		Int64("bytesDown", snapshot.BytesDown).
		Int64("uptimeSeconds", stats.UptimeSeconds()).
		Msg("Relay counters")
}
