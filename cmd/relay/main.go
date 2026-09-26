// Command relay runs a Blade L4 relay node: it terminates no TLS, it just
// routes connections by the SNI in the ClientHello and copies bytes to an
// origin, while announcing itself to Blade's service registry.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
	"srv.solsynth.dev/sosys/blade/internal/relay"
)

const shutdownTimeout = 5 * time.Second

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
		Str("publicAddress", net.JoinHostPort(cfg.Relay.PublicHost, strconv.Itoa(cfg.Relay.PublicPort))).
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

	healthServer := &http.Server{
		Addr:    cfg.Health.Listen,
		Handler: relay.NewHealthHandler(*cfg, stats),
	}
	go func() {
		logging.Log.Info().Str("listen", cfg.Health.Listen).Msg("Starting relay health server")
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Log.Fatal().Err(err).Str("listen", cfg.Health.Listen).Msg("Failed to start relay health server")
		}
	}()

	go func() {
		logging.Log.Info().Str("listen", cfg.Relay.Listen).Msg("Relay is accepting connections")
		if err := server.Serve(listener); err != nil {
			logging.Log.Fatal().Err(err).Msg("Relay server stopped")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var registration *relay.Registration
	if cfg.Discovery.Enabled {
		conn, err := grpc.NewClient(cfg.Discovery.Target, grpc.WithTransportCredentials(discoveryCredentials(*cfg)))
		if err != nil {
			logging.Log.Fatal().Err(err).Str("target", cfg.Discovery.Target).Msg("Failed to create the discovery client")
		}
		defer func() { _ = conn.Close() }()

		registration = relay.NewRegistration(gen.NewDyServiceDiscoveryServiceClient(conn), *cfg)
		go registration.Run(ctx)
		logging.Log.Info().
			Str("target", cfg.Discovery.Target).
			Str("service", cfg.Discovery.Service).
			Dur("lease", time.Duration(cfg.Discovery.LeaseSeconds)*time.Second).
			Msg("Announcing this relay to Blade service discovery")
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
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		logging.Log.Warn().Err(err).Msg("Relay health server shutdown was incomplete")
	}

	logging.Log.Info().Msg("Relay exited")
}

func discoveryCredentials(cfg relay.Config) credentials.TransportCredentials {
	if !cfg.Discovery.UseTLS {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: cfg.Discovery.TLSSkipVerify,
		ServerName:         cfg.Discovery.TLSServerName,
	})
}
