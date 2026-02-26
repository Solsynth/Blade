package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.solsynth.dev/solarnetwork/blade/internal/config"
	"git.solsynth.dev/solarnetwork/blade/internal/health"
	"git.solsynth.dev/solarnetwork/blade/internal/logging"
	"git.solsynth.dev/solarnetwork/blade/internal/middleware"
	"git.solsynth.dev/solarnetwork/blade/internal/proxy"
	"git.solsynth.dev/solarnetwork/blade/internal/wsgateway"
	"github.com/gin-gonic/gin"
)

func main() {
	pretty := os.Getenv("GIN_MODE") == "debug" || os.Getenv("ZEROLOG_PRETTY") == "true"
	logging.Init(pretty)

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/config.toml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logging.Log.Fatal().Err(err).Msg("Failed to load config")
	}

	logging.Log.Info().
		Str("configPath", configPath).
		Int("specialRoutes", len(cfg.SpecialRoutes.Routes)).
		Msg("Starting Blade Gateway")
	for _, route := range cfg.SpecialRoutes.Routes {
		logging.Log.Info().
			Str("path", route.Path).
			Str("service", route.Service).
			Str("target", route.Target).
			Bool("prefix", route.Prefix).
			Msg("Configured special route")
	}

	store := health.NewReadinessStore(cfg.Endpoints.CoreServiceNames)
	aggregator := health.NewAggregator(store, cfg)

	go aggregator.Start(context.Background())

	proxyHandler := proxy.New(cfg)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	r.Use(middleware.CORS())

	r.Use(health.ReadinessMiddleware(store))

	if cfg.WebSocketGateway.Enabled {
		authService := cfg.WebSocketGateway.AuthService
		if authService == "" {
			authService = "pass"
		}
		authTarget := config.GetServiceGRPC(authService)
		if authTarget == "" {
			logging.Log.Fatal().Str("service", authService).Msg("WebSocket gateway auth service GRPC endpoint is not configured")
		}

		allowedAlt := make(map[string]struct{}, len(cfg.WebSocketGateway.AllowedDeviceAltern))
		for _, alt := range cfg.WebSocketGateway.AllowedDeviceAltern {
			allowedAlt[alt] = struct{}{}
		}

		wsCfg := wsgateway.Config{
			KeepAliveInterval: time.Duration(cfg.WebSocketGateway.KeepAliveSeconds) * time.Second,
			MaxMessageBytes:   cfg.WebSocketGateway.MaxMessageBytes,
			AllowedDeviceAlt:  allowedAlt,
		}
		wsService := wsgateway.NewService(wsCfg, nil, nil, nil)
		wsAuth, err := wsgateway.NewGRPCTokenAuthenticator(authTarget)
		if err != nil {
			logging.Log.Fatal().Err(err).Str("authTarget", authTarget).Msg("Failed to create websocket auth client")
		}
		wsHandler := wsgateway.NewHTTPHandler(wsAuth, wsService, wsCfg)
		wsPath := cfg.WebSocketGateway.Path
		if wsPath == "" {
			wsPath = "/ws"
		}
		r.GET(wsPath, wsHandler.Handle)

		logging.Log.Info().Str("path", wsPath).Str("authService", authService).Str("authTarget", authTarget).Msg("WebSocket gateway enabled")
	}

	r.NoRoute(proxyHandler.Handler())

	r.GET("/health", func(c *gin.Context) {
		states := store.GetAllStates()
		coreServiceHealthy := store.IsCoreServiceHealthy()

		allHealthy := true
		for _, state := range states {
			if !state.IsHealthy {
				allHealthy = false
				break
			}
		}

		if !coreServiceHealthy {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status":     states,
				"ready":      coreServiceHealthy,
				"aggregated": allHealthy,
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status":     states,
			"ready":      coreServiceHealthy,
			"aggregated": allHealthy,
		})
	})

	addr := ":" + cfg.Server.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      r.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout * time.Second,
		WriteTimeout: cfg.Server.WriteTimeout * time.Second,
	}

	go func() {
		logging.Log.Info().Str("port", cfg.Server.Port).Msg("Starting HTTP server")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Log.Fatal().Err(err).Msg("Failed to start server")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logging.Log.Info().Msg("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logging.Log.Fatal().Err(err).Msg("Server forced to shutdown")
	}

	logging.Log.Info().Msg("Server exited")
}
