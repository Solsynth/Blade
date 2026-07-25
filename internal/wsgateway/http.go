package wsgateway

import (
	"fmt"
	"net/http"
	"time"

	"srv.solsynth.dev/sosys/blade/internal/logging"
	"src.solsynth.dev/sosys/go/pkg/cache"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/websocket"
)

type HttpHandler struct {
	authenticator dyauth.TokenAuthenticator
	service       *Service
	cfg           Config
	cache         cache.CacheService
	profiles      gen.DyProfileServiceClient
}

func NewHttpHandler(authenticator dyauth.TokenAuthenticator, service *Service, cfg Config, c cache.CacheService, profiles gen.DyProfileServiceClient) *HttpHandler {
	if cfg.KeepAliveInterval <= 0 {
		cfg.KeepAliveInterval = 60 * time.Second
	}

	return &HttpHandler{
		authenticator: authenticator,
		service:       service,
		cfg:           cfg,
		cache:         c,
		profiles:      profiles,
	}
}

func (h *HttpHandler) Handle(c *gin.Context) {
	requestPath := c.Request.URL.Path
	requestQuery := c.Request.URL.RawQuery
	requestOrigin := c.Request.Header.Get("Origin")

	namespace := c.Query("namespace")
	if namespace == "" {
		namespace = h.cfg.DefaultNamespace
	}

	deviceAlt := c.Query("deviceAlt")
	if deviceAlt != "" {
		nsCfg := h.resolveNamespaceConfig(namespace)
		if _, ok := nsCfg.AllowedDeviceAlt[deviceAlt]; !ok {
			logging.Log.Warn().
				Str("path", requestPath).
				Str("origin", requestOrigin).
				Str("namespace", namespace).
				Str("deviceAlt", deviceAlt).
				Msg("Rejected websocket request due to unsupported deviceAlt")
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported deviceAlt"})
			return
		}
	}

	auth, err := dyauth.AuthenticateRequest(c.Request.Context(), h.authenticator, c.Request)
	if err != nil {
		logging.Log.Warn().
			Err(err).
			Str("path", requestPath).
			Str("query", requestQuery).
			Str("origin", requestOrigin).
			Str("namespace", namespace).
			Msg("Rejected websocket request due to authentication failure")
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	tokenInfo, _ := dyauth.ExtractToken(c.Request)

	// Hydrate profile and touch last-seen (best-effort)
	_ = dyauth.HydrateAndTouch(c.Request.Context(), h.cache, h.profiles, auth)

	deviceID := auth.Session.GetClientId()
	if deviceAlt != "" {
		deviceID = deviceID + "+" + deviceAlt
	}

	server := websocket.Server{
		Handshake: func(cfg *websocket.Config, req *http.Request) error {
			// Allow browser and non-browser clients (some do not send Origin).
			if _, err := websocket.Origin(cfg, req); err != nil {
				logging.Log.Warn().
					Err(err).
					Str("path", requestPath).
					Str("origin", requestOrigin).
					Str("namespace", namespace).
					Str("accountId", auth.Account.GetId()).
					Str("deviceId", deviceID).
					Msg("Rejected websocket handshake due to invalid origin")
				return fmt.Errorf("invalid websocket origin: %w", err)
			}
			return nil
		},
		Handler: websocket.Handler(func(conn *websocket.Conn) {
			logging.Log.Info().
				Str("namespace", namespace).
				Str("accountId", auth.Account.GetId()).
				Str("deviceId", deviceID).
				Str("sessionId", auth.Session.GetId()).
				Str("origin", requestOrigin).
				Str("path", requestPath).
				Msg("Upgraded websocket connection")
			h.service.HandleConnection(c.Request.Context(), SessionAuthContext{
				Account:   auth.Account,
				Session:   auth.Session,
				TokenInfo: tokenInfo,
				Request:   dyauth.CloneRequestMetadata(c.Request),
			}, namespace, deviceID, conn)
		}),
	}

	server.ServeHTTP(c.Writer, c.Request)

	logging.Log.Debug().
		Str("namespace", namespace).
		Str("accountId", auth.Account.GetId()).
		Str("deviceId", deviceID).
		Msg("Websocket handler completed")
}

func (h *HttpHandler) resolveNamespaceConfig(namespace string) NamespaceConfig {
	if ns, ok := h.cfg.Namespaces[namespace]; ok {
		if ns.KeepAliveInterval <= 0 {
			ns.KeepAliveInterval = h.cfg.KeepAliveInterval
		}
		if ns.MaxMessageBytes <= 0 {
			ns.MaxMessageBytes = h.cfg.MaxMessageBytes
		}
		if ns.AllowedDeviceAlt == nil {
			ns.AllowedDeviceAlt = h.cfg.AllowedDeviceAlt
		}
		return ns
	}
	return NamespaceConfig{
		KeepAliveInterval: h.cfg.KeepAliveInterval,
		MaxMessageBytes:   h.cfg.MaxMessageBytes,
		AllowedDeviceAlt:  h.cfg.AllowedDeviceAlt,
	}
}
