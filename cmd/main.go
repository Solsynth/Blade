package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dysonnetwork/gateway/internal/config"
	"github.com/dysonnetwork/gateway/internal/health"
	"github.com/dysonnetwork/gateway/internal/logging"
	"github.com/dysonnetwork/gateway/internal/middleware"
	"github.com/dysonnetwork/gateway/internal/proxy"
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

	logging.Log.Info().Msg("Starting Dyson Network Gateway")

	store := health.NewReadinessStore(cfg.Endpoints.CoreServiceNames)
	aggregator := health.NewAggregator(store, cfg)

	go aggregator.Start(context.Background())

	rateLimiter := middleware.NewRateLimiter(
		cfg.RateLimit.RequestsPerMinute,
		cfg.RateLimit.BurstAllowance,
	)

	proxyHandler := proxy.New(cfg)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	r.Use(middleware.CORS())

	r.Use(rateLimiter.Middleware())

	r.Use(health.ReadinessMiddleware(store))

	r.NoRoute(proxyHandler.Handler())

	r.GET("/health", func(c *gin.Context) {
		states := store.GetAllStates()
		allHealthy := true
		for _, state := range states {
			if !state.IsHealthy {
				allHealthy = false
				break
			}
		}

		status := http.StatusOK
		if !allHealthy {
			status = http.StatusServiceUnavailable
		}

		c.JSON(status, gin.H{
			"status":     states,
			"ready":      store.IsCoreServiceHealthy(),
			"aggregated": allHealthy,
		})
	})

	addr := ":" + cfg.Server.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
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
