package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	eb "src.solsynth.dev/sosys/go/pkg/eventbus"
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
	var relayCatalog *discovery.Catalog
	var relayAPI *discovery.RelayAPI
	var capabilityAggregator *capabilities.Aggregator
	if cfg.Discovery.Enabled {
		if redisClient == nil {
			logging.Log.Fatal().Msg("Service discovery requires cache.redisUrl")
		}
		if strings.TrimSpace(cfg.Discovery.RegistrationToken) == "" {
			logging.Log.Fatal().Msg("Service discovery requires discovery.registrationToken")
		}
		registry = discovery.NewRegistry(redisClient, cfg.Discovery.Prefix, time.Duration(cfg.Discovery.LeaseSeconds)*time.Second)
		relayCatalog = discovery.NewCatalog(registry, cfg.Discovery.RelayServiceName)
		relayAPI = discovery.NewRelayAPI(
			registry,
			cfg.Discovery.RelayServiceName,
			cfg.Discovery.RegistrationToken,
			time.Duration(cfg.Discovery.LeaseSeconds)*time.Second,
		)
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
	var natsBus *eb.Bus
	var wsPushPublisher wsgateway.PushPublisher

	r := gin.New()
	// Blade resolves the client address itself (proxy.ResolveClientIP) for both
	// the forwarded headers and the access log, so gin must not trust any
	// client-supplied X-Forwarded-For: a spoofed value would otherwise reach
	// c.ClientIP() and any handler that uses it.
	if err := r.SetTrustedProxies(nil); err != nil {
		logging.Log.Fatal().Err(err).Msg("Failed to disable gin trusted proxy headers")
	}
	r.Use(gin.Recovery())
	r.Use(gin.LoggerWithFormatter(accessLogFormatter(cfg.Proxy.TrustedProxyHops)))
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

	// The relay control plane and the health endpoint are mounted ahead of the
	// readiness gate on purpose: relays live outside the cluster network and
	// must be able to register, renew, and withdraw even while every core
	// service is down, and /health has to answer with a health document
	// (draft-inadarei-api-health-check-06) rather than the gate's generic 503.
	if relayAPI != nil {
		relayAPI.RegisterRoutes(r)
	}

	registerHealthRoutes(r, store, cfg)

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
		}

		var forwarder wsgateway.UnknownPacketForwarder
		var eventPublisher wsgateway.ConnectionEventPublisher
		natsURL := cfg.NATS.URL
		if natsURL != "" {
			natsBus, err = connectNATSWithRetry(natsURL)
			if err != nil {
				logging.Log.Fatal().Err(err).Str("natsURL", natsURL).Msg("Failed to connect to NATS")
			}
			natsForwarder := wsgateway.NewNatsForwarder(natsBus.Conn, wsgateway.NATSForwarderConfig{
				SubjectPrefix: cfg.NATS.WebSocketSubjectPrefix,
			})
			forwarder = natsForwarder
			eventPublisher = natsForwarder
			wsPushPublisher = wsgateway.NewNATSPushPublisher(natsBus.Conn, cfg.NATS.WebSocketSubjectPrefix)
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
		if natsBus != nil {
			if _, err := wsgateway.SubscribeWebSocketPushes(natsBus.Conn, cfg.NATS.WebSocketSubjectPrefix, wsService); err != nil {
				logging.Log.Fatal().Err(err).Msg("Failed to subscribe to websocket push events")
			}
			if _, err := wsgateway.SubscribeAuthSessionRevocations(natsBus.Conn, wsService); err != nil {
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

	registerRelaysRoute(r, relayCatalog)

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
	if natsBus != nil {
		natsBus.Conn.Close()
	}
	if redisClient != nil {
		_ = redisClient.Close()
	}

	logging.Log.Info().Msg("Server exited")
}

// accessLogFormatter renders the same access log line as gin's default logger
// but reports the address resolved by the gateway (proxy.ResolveClientIP)
// instead of gin's c.ClientIP(). Gin's ClientIP honours X-Forwarded-For, so
// without this a client could forge the address recorded in the access log,
// which is the audit trail.
func accessLogFormatter(trustedProxyHops int) gin.LogFormatter {
	return func(p gin.LogFormatterParams) string {
		latency := p.Latency
		if latency > time.Minute {
			latency = latency.Truncate(time.Second)
		}
		return fmt.Sprintf("[GIN] %v | %3d | %13v | %15s |%-7s %#v\n%s",
			p.TimeStamp.Format("2006/01/02 - 15:04:05"),
			p.StatusCode,
			latency,
			proxy.ResolveClientIP(p.Request, trustedProxyHops),
			p.Method,
			p.Path,
			p.ErrorMessage,
		)
	}
}

// registerHealthRoutes mounts the gateway's health documents.
//
// Both routes are public and mounted ahead of the readiness gate. The full
// document — including the per-service checks map — is served to everyone by
// design: it is the client-facing status surface the app polls, so the tracked
// service names are disclosed to anonymous callers and that disclosure is an
// accepted risk, not operator-only topology.
func registerHealthRoutes(r *gin.Engine, store *health.ReadinessStore, cfg *config.Config) {
	r.GET("/health", func(c *gin.Context) {
		response := health.BuildResponse(store, healthBaseURL(c))
		c.Header("Content-Type", health.MediaTypeHealthJSON)
		c.Header("Cache-Control", fmt.Sprintf("max-age=%d", cfg.Health.CheckIntervalSeconds))
		c.JSON(response.HTTPStatus(), response)
	})

	// The per-service form of the same document. It is public on purpose: it is
	// what a status page (and the per-check "self" links) polls, and it must
	// report an unhealthy service as a failing document rather than a bare code.
	r.GET("/health/:service", func(c *gin.Context) {
		response, tracked := health.BuildServiceResponse(store, c.Param("service"), healthBaseURL(c))
		status := response.HTTPStatus()
		if !tracked {
			status = http.StatusNotFound
		}
		c.Header("Content-Type", health.MediaTypeHealthJSON)
		c.Header("Cache-Control", fmt.Sprintf("max-age=%d", cfg.Health.CheckIntervalSeconds))
		c.JSON(status, response)
	})
}

// registerRelaysRoute mounts the public relay catalog.
//
// GET /relays is an intentionally public, client-facing discovery contract, not
// infrastructure inventory: clients dial the returned host:port and sort the
// picker by region, weight, and health, so the fields cannot be hidden or
// gated. The only protected routes are the control plane's PUT/DELETE
// /relays/{id}, which relays authenticate with discovery.registrationToken.
func registerRelaysRoute(r *gin.Engine, catalog *discovery.Catalog) {
	r.GET("/relays", func(c *gin.Context) {
		if catalog == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service discovery is disabled"})
			return
		}
		relays, err := catalog.List(c.Request.Context())
		if err != nil {
			logging.Log.Warn().Err(err).Msg("Failed to list relays")
			c.JSON(http.StatusBadGateway, gin.H{"error": "unable to list relays"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"relays": relays})
	})
}

// healthBaseURL rebuilds the origin the client used to reach the gateway,
// honouring the headers a public edge proxy sets, so the health document can
// publish absolute "self" links. It is empty when no host is known, in which
// case the document carries no links rather than a guessed one.
func healthBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := forwardedHeader(c, "X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := forwardedHeader(c, "X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func forwardedHeader(c *gin.Context, name string) string {
	value := c.GetHeader(name)
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(value, ",")[0])
}

func connectNATSWithRetry(natsURL string) (*eb.Bus, error) {
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
		conn, err := eb.Connect(normalizedURL, opts...)
		if err == nil {
			logging.Log.Info().
				Str("natsURL", normalizedURL).
				Str("status", conn.Conn.Status().String()).
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
