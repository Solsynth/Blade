package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.solsynth.dev/sosys/blade/internal/config"
	"git.solsynth.dev/sosys/blade/internal/health"
	"git.solsynth.dev/sosys/blade/internal/logging"
	"git.solsynth.dev/sosys/blade/internal/proxy"
	"git.solsynth.dev/sosys/blade/internal/wsgateway"
	dyauth "git.solsynth.dev/sosys/blade/pkg/auth"
	gen "git.solsynth.dev/sosys/spec/gen/go"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
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
		Int("routes", len(cfg.Routes)).
		Msg("Starting Blade Gateway")
	for _, route := range cfg.Routes {
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
	var wsService *wsgateway.Service
	var natsConn *nats.Conn

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())
	isDebugMode := gin.Mode() == gin.DebugMode

	r.Use(cors.New(cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization", "X-Client-Ability", "User-Agent"},
		ExposeHeaders:    []string{"Content-Length", "X-Total", "X-NotReady"},
		AllowCredentials: true,
		AllowOriginFunc: func(origin string) bool {
			return true
		},
		MaxAge: 12 * time.Hour,
	}))

	r.Use(health.ReadinessMiddleware(store))

	if cfg.WebSocket.Enabled {
		authService := cfg.WebSocket.AuthService
		authGrpcTarget := config.GetServiceGrpc(authService)
		if authGrpcTarget == "" {
			logging.Log.Fatal().
				Str("authService", authService).
				Msg("WebSocket gateway enabled but auth service gRPC target is missing")
		}

		authenticator, err := dyauth.NewGrpcTokenAuthenticator(dyauth.GrpcAuthDialConfig{
			Target:        authGrpcTarget,
			UseTLS:        cfg.WebSocket.AuthUseTLS,
			TLSSkipVerify: cfg.WebSocket.AuthTLSSkipVerify,
			TLSServerName: cfg.WebSocket.AuthTLSServerName,
		})
		if err != nil {
			logging.Log.Fatal().
				Err(err).
				Str("authService", authService).
				Str("grpcTarget", authGrpcTarget).
				Msg("Failed to initialize websocket token authenticator")
		}

		wsCfg := wsgateway.Config{
			KeepAliveInterval: time.Duration(cfg.WebSocket.KeepAliveSeconds) * time.Second,
			MaxMessageBytes:   cfg.WebSocket.MaxMessageBytes,
			AllowedDeviceAlt:  make(map[string]struct{}, len(cfg.WebSocket.AllowedDeviceAltern)),
		}
		for _, alt := range cfg.WebSocket.AllowedDeviceAltern {
			wsCfg.AllowedDeviceAlt[alt] = struct{}{}
		}

		var forwarder wsgateway.UnknownPacketForwarder
		natsURL := cfg.NATS.URL
		if natsURL != "" {
			natsConn, err = nats.Connect(natsURL)
			if err != nil {
				logging.Log.Fatal().Err(err).Str("natsURL", natsURL).Msg("Failed to connect to NATS")
			}
			forwarder = wsgateway.NewNatsForwarder(natsConn, wsgateway.NATSForwarderConfig{
				SubjectPrefix: cfg.NATS.WebSocketSubjectPrefix,
			})
			logging.Log.Info().
				Str("natsURL", natsURL).
				Str("subjectPrefix", cfg.NATS.WebSocketSubjectPrefix).
				Msg("Enabled websocket unknown packet forwarder via NATS")
		} else {
			logging.Log.Warn().Msg("NATS URL is empty; websocket unknown packet forwarding is disabled")
		}

		wsService = wsgateway.NewService(wsCfg, nil, forwarder, nil)
		wsHandler := wsgateway.NewHttpHandler(authenticator, wsService, wsCfg)
		r.GET(cfg.WebSocket.Path, wsHandler.Handle)

		if isDebugMode {
			debugWs := r.Group("/debug/ws")
			debugWs.GET("/summary", func(c *gin.Context) {
				users := wsService.GetAllConnectedUserIDs()
				devices := wsService.GetAllConnectedDeviceIDs()
				c.JSON(http.StatusOK, gin.H{
					"enabled":         true,
					"path":            cfg.WebSocket.Path,
					"connectionCount": len(wsService.GetConnectionSnapshots()),
					"userCount":       len(users),
					"deviceCount":     len(devices),
					"users":           users,
					"devices":         devices,
				})
			})
			debugWs.GET("/connections", func(c *gin.Context) {
				connections := wsService.GetConnectionSnapshots()
				c.JSON(http.StatusOK, gin.H{
					"count":       len(connections),
					"connections": connections,
				})
			})
			debugWs.GET("/account/:accountId", func(c *gin.Context) {
				accountID := c.Param("accountId")
				devices := wsService.GetDevicesByAccount(accountID)
				c.JSON(http.StatusOK, gin.H{
					"accountId":   accountID,
					"connected":   len(devices) > 0,
					"deviceCount": len(devices),
					"devices":     devices,
				})
			})
			debugWs.GET("/device/:deviceId", func(c *gin.Context) {
				deviceID := c.Param("deviceId")
				accounts := wsService.GetAccountsByDevice(deviceID)
				c.JSON(http.StatusOK, gin.H{
					"deviceId":     deviceID,
					"connected":    len(accounts) > 0,
					"accountCount": len(accounts),
					"accounts":     accounts,
				})
			})

			logging.Log.Info().Msg("Registered debug websocket endpoints under /debug/ws")
		}

		logging.Log.Info().
			Str("path", cfg.WebSocket.Path).
			Str("authService", authService).
			Str("authGrpcTarget", authGrpcTarget).
			Bool("authUseTLS", cfg.WebSocket.AuthUseTLS).
			Bool("authTLSSkipVerify", cfg.WebSocket.AuthTLSSkipVerify).
			Str("authTLSServerName", cfg.WebSocket.AuthTLSServerName).
			Int64("maxMessageBytes", cfg.WebSocket.MaxMessageBytes).
			Msg("Registered websocket gateway route")
	}

	r.NoRoute(proxyHandler.Handler())

	r.GET("/config/site", func(c *gin.Context) {
		c.String(http.StatusOK, cfg.SiteURL)
	})

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

	var grpcSrv *grpc.Server
	if cfg.GRPC.Enabled && wsService != nil {
		grpcAddr := ":" + cfg.GRPC.Port
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			logging.Log.Fatal().Err(err).Str("port", cfg.GRPC.Port).Msg("Failed to listen gRPC server")
		}

		grpcSrv = grpc.NewServer()
		gen.RegisterWebSocketServiceServer(grpcSrv, wsgateway.NewGRPCService(wsService))
		reflection.Register(grpcSrv)

		go func() {
			logging.Log.Info().Str("port", cfg.GRPC.Port).Msg("Starting gRPC server")
			if err := grpcSrv.Serve(lis); err != nil {
				logging.Log.Fatal().Err(err).Msg("Failed to start gRPC server")
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logging.Log.Info().Msg("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logging.Log.Fatal().Err(err).Msg("Server forced to shutdown")
	}
	if grpcSrv != nil {
		gracefulStopped := make(chan struct{})
		go func() {
			grpcSrv.GracefulStop()
			close(gracefulStopped)
		}()

		select {
		case <-gracefulStopped:
		case <-ctx.Done():
			grpcSrv.Stop()
		}
	}
	if natsConn != nil {
		natsConn.Close()
	}

	logging.Log.Info().Msg("Server exited")
}
