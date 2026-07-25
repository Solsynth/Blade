package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	"src.solsynth.dev/sosys/go/pkg/cache"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/capabilities"
	"srv.solsynth.dev/sosys/blade/internal/config"
	discovery "srv.solsynth.dev/sosys/blade/internal/discovery"
	"srv.solsynth.dev/sosys/blade/internal/health"
	"srv.solsynth.dev/sosys/blade/internal/logging"
	"srv.solsynth.dev/sosys/blade/internal/proxy"
	"srv.solsynth.dev/sosys/blade/internal/wsgateway"
)

const (
	natsInitialRetryWait = 2 * time.Second
	natsReconnectWait    = 2 * time.Second
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

	var redisClient *redis.Client
	if cfg.Cache.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.Cache.RedisURL)
		if err != nil {
			logging.Log.Fatal().Err(err).Str("redisUrl", cfg.Cache.RedisURL).Msg("Failed to parse Redis URL")
		}
		redisClient = redis.NewClient(opt)
	}

	var registry *discovery.Registry
	var capabilityAggregator *capabilities.Aggregator
	if cfg.Discovery.Enabled {
		if redisClient == nil {
			logging.Log.Fatal().Msg("Service discovery requires cache.redisUrl")
		}
		if strings.TrimSpace(cfg.Discovery.RegistrationToken) == "" {
			logging.Log.Fatal().Msg("Service discovery requires discovery.registrationToken")
		}
		registry = discovery.NewRegistry(redisClient, cfg.Discovery.Prefix, time.Duration(cfg.Discovery.LeaseSeconds)*time.Second)
		capabilityAggregator = capabilities.NewWithTLSConfig(registry, cfg.GRPC.ClientTLSSkipVerify, cfg.Endpoints.CoreServiceNames...)
		logging.Log.Info().Str("prefix", cfg.Discovery.Prefix).Msg("Enabled Redis-backed service discovery")
	}

	store := health.NewReadinessStore(cfg.Endpoints.CoreServiceNames)
	aggregator := health.NewAggregator(store, cfg, registry)

	go aggregator.Start(context.Background())
	if capabilityAggregator != nil {
		go capabilityAggregator.Start(context.Background())
	}

	proxyHandler := proxy.New(cfg, registry)
	var wsService *wsgateway.Service
	var natsConn *nats.Conn
	var wsPushPublisher wsgateway.PushPublisher

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

		// Initialize cache service
		var cacheSvc cache.CacheService
		if redisClient != nil {
			cacheSvc = cache.NewRedisCacheService(redisClient)
			logging.Log.Info().Str("redisUrl", cfg.Cache.RedisURL).Msg("Using Redis cache for auth sessions")
		} else {
			cacheSvc = cache.NewMemoryCacheService(10000)
			logging.Log.Info().Msg("Using in-memory LRU cache for auth sessions")
		}

		// Wrap authenticator with session caching
		cachedAuth := dyauth.NewCachedTokenAuthenticator(authenticator, cacheSvc)

		// Initialize profile service gRPC connection
		profileService := cfg.WebSocket.ProfileService
		profileGrpcTarget := config.GetServiceGrpc(profileService)
		if profileGrpcTarget == "" {
			logging.Log.Fatal().
				Str("profileService", profileService).
				Msg("WebSocket gateway enabled but profile service gRPC target is missing")
		}

		profileTarget, profileUseTLS := dyauth.NormalizeAuthGRPCTarget(profileGrpcTarget, cfg.WebSocket.ProfileUseTLS)
		profileGrpcConn, err := grpc.Dial(profileTarget, grpc.WithTransportCredentials(
			func() credentials.TransportCredentials {
				if profileUseTLS {
					return credentials.NewTLS(&tls.Config{InsecureSkipVerify: cfg.WebSocket.ProfileTLSSkipVerify})
				}
				return insecure.NewCredentials()
			}(),
		))
		if err != nil {
			logging.Log.Fatal().Err(err).Str("target", profileGrpcTarget).Msg("Failed to dial profile service")
		}
		profileClient := gen.NewDyProfileServiceClient(profileGrpcConn)

		wsCfg := wsgateway.Config{
			KeepAliveInterval: time.Duration(cfg.WebSocket.KeepAliveSeconds) * time.Second,
			MaxMessageBytes:   cfg.WebSocket.MaxMessageBytes,
			DefaultNamespace:  cfg.WebSocket.DefaultNamespace,
			AllowedDeviceAlt:  make(map[string]struct{}, len(cfg.WebSocket.AllowedDeviceAltern)),
		}
		for _, alt := range cfg.WebSocket.AllowedDeviceAltern {
			wsCfg.AllowedDeviceAlt[alt] = struct{}{}
		}

		var forwarder wsgateway.UnknownPacketForwarder
		var eventPublisher wsgateway.ConnectionEventPublisher
		natsURL := cfg.NATS.URL
		if natsURL != "" {
			natsConn, err = connectNATSWithRetry(natsURL)
			if err != nil {
				logging.Log.Fatal().Err(err).Str("natsURL", natsURL).Msg("Failed to connect to NATS")
			}
			natsForwarder := wsgateway.NewNatsForwarder(natsConn, wsgateway.NATSForwarderConfig{
				SubjectPrefix: cfg.NATS.WebSocketSubjectPrefix,
			})
			forwarder = natsForwarder
			eventPublisher = natsForwarder
			wsPushPublisher = wsgateway.NewNATSPushPublisher(natsConn, cfg.NATS.WebSocketSubjectPrefix)
			logging.Log.Info().
				Str("natsURL", natsURL).
				Str("subjectPrefix", cfg.NATS.WebSocketSubjectPrefix).
				Msg("Enabled websocket NATS forwarding and connection events")
		} else {
			logging.Log.Warn().Msg("NATS URL is empty; websocket unknown packet forwarding and connection events are disabled")
		}

		wsService = wsgateway.NewService(wsCfg, nil, forwarder, eventPublisher, cachedAuth, cacheSvc, profileClient)
		if redisClient != nil {
			wsService.SetPresence(wsgateway.NewRedisPresenceStore(redisClient, "", 2*time.Minute))
			logging.Log.Info().Msg("Enabled Redis-backed websocket presence")
		} else {
			logging.Log.Warn().Msg("Redis is not configured; websocket presence remains local to each gateway replica")
		}
		if natsConn != nil {
			if _, err := wsgateway.SubscribeWebSocketPushes(natsConn, cfg.NATS.WebSocketSubjectPrefix, wsService); err != nil {
				logging.Log.Fatal().Err(err).Msg("Failed to subscribe to websocket push events")
			}
			if _, err := wsgateway.SubscribeAuthSessionRevocations(natsConn, wsService); err != nil {
				logging.Log.Fatal().Err(err).Msg("Failed to subscribe to auth session revocation events")
			}
			logging.Log.Info().
				Str("subject", wsgateway.AuthSessionRevokedSubject).
				Msg("Subscribed to auth session revocation events")
		}
		wsHandler := wsgateway.NewHttpHandler(cachedAuth, wsService, wsCfg, cacheSvc, profileClient)
		r.GET(cfg.WebSocket.Path, wsHandler.Handle)

		if isDebugMode {
			debugWs := r.Group("/debug/ws")
			debugWs.GET("/summary", func(c *gin.Context) {
				namespace := c.DefaultQuery("namespace", "")
				users := wsService.GetAllConnectedUserIDs(namespace)
				devices := wsService.GetAllConnectedDeviceIDs(namespace)
				c.JSON(http.StatusOK, gin.H{
					"enabled":         true,
					"path":            cfg.WebSocket.Path,
					"namespace":       namespace,
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
				namespace := c.DefaultQuery("namespace", "")
				accountID := c.Param("accountId")
				devices := wsService.GetDevicesByAccount(namespace, accountID)
				c.JSON(http.StatusOK, gin.H{
					"namespace":   namespace,
					"accountId":   accountID,
					"connected":   len(devices) > 0,
					"deviceCount": len(devices),
					"devices":     devices,
				})
			})
			debugWs.GET("/device/:deviceId", func(c *gin.Context) {
				namespace := c.DefaultQuery("namespace", "")
				deviceID := c.Param("deviceId")
				accounts := wsService.GetAccountsByDevice(namespace, deviceID)
				c.JSON(http.StatusOK, gin.H{
					"namespace":    namespace,
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

	r.GET("/meta", func(c *gin.Context) {
		if capabilityAggregator == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service discovery is disabled"})
			return
		}
		c.JSON(http.StatusOK, capabilityAggregator.Document())
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
	if cfg.GRPC.Enabled && (wsService != nil || registry != nil) {
		grpcAddr := ":" + cfg.GRPC.Port
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			logging.Log.Fatal().Err(err).Str("port", cfg.GRPC.Port).Msg("Failed to listen gRPC server")
		}

		grpcSrv = grpc.NewServer()
		if wsService != nil {
			grpcWSService := wsgateway.NewGRPCService(wsService)
			grpcWSService.SetPushPublisher(wsPushPublisher)
			gen.RegisterWebSocketServiceServer(grpcSrv, grpcWSService)
		}
		if registry != nil {
			gen.RegisterDyServiceDiscoveryServiceServer(grpcSrv, discovery.NewGRPCService(registry, cfg.Discovery.RegistrationToken))
		}
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
	if redisClient != nil {
		_ = redisClient.Close()
	}

	logging.Log.Info().Msg("Server exited")
}

func connectNATSWithRetry(natsURL string) (*nats.Conn, error) {
	normalizedURL := strings.TrimSpace(natsURL)
	if normalizedURL == "" {
		return nil, errors.New("nats URL is empty")
	}

	opts := []nats.Option{
		nats.Name("blade-gateway"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(natsReconnectWait),
		nats.DisconnectErrHandler(func(conn *nats.Conn, err error) {
			log := logging.Log.Warn().Str("server", conn.ConnectedUrl())
			if err != nil {
				log = log.Err(err)
			}
			log.Msg("Disconnected from NATS; waiting to reconnect")
		}),
		nats.ReconnectHandler(func(conn *nats.Conn) {
			logging.Log.Info().
				Str("server", conn.ConnectedUrl()).
				Msg("Reconnected to NATS")
		}),
		nats.ClosedHandler(func(conn *nats.Conn) {
			log := logging.Log.Warn()
			if lastErr := conn.LastError(); lastErr != nil {
				log = log.Err(lastErr)
			}
			log.Msg("NATS connection closed")
		}),
	}

	for {
		conn, err := nats.Connect(normalizedURL, opts...)
		if err == nil {
			logging.Log.Info().
				Str("natsURL", normalizedURL).
				Str("status", conn.Status().String()).
				Msg("Initialized NATS connection")
			return conn, nil
		}

		logging.Log.Warn().
			Err(err).
			Str("natsURL", normalizedURL).
			Dur("retryIn", natsInitialRetryWait).
			Msg("NATS not ready yet; retrying connection")
		time.Sleep(natsInitialRetryWait)
	}
}
