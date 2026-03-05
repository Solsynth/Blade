package wsgateway

import (
	"fmt"
	"net/http"
	"time"

	"git.solsynth.dev/sosys/blade/internal/logging"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/websocket"
)

type HttpHandler struct {
	authenticator TokenAuthenticator
	service       *Service
	cfg           Config
}

func NewHttpHandler(authenticator TokenAuthenticator, service *Service, cfg Config) *HttpHandler {
	if cfg.KeepAliveInterval <= 0 {
		cfg.KeepAliveInterval = 60 * time.Second
	}

	return &HttpHandler{
		authenticator: authenticator,
		service:       service,
		cfg:           cfg,
	}
}

func (h *HttpHandler) Handle(c *gin.Context) {
	requestPath := c.Request.URL.Path
	requestQuery := c.Request.URL.RawQuery
	requestOrigin := c.Request.Header.Get("Origin")

	deviceAlt := c.Query("deviceAlt")
	if deviceAlt != "" {
		if _, ok := h.cfg.AllowedDeviceAlt[deviceAlt]; !ok {
			logging.Log.Warn().
				Str("path", requestPath).
				Str("origin", requestOrigin).
				Str("deviceAlt", deviceAlt).
				Msg("Rejected websocket request due to unsupported deviceAlt")
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported deviceAlt"})
			return
		}
	}

	auth, err := authenticateRequest(c.Request.Context(), h.authenticator, c.Request)
	if err != nil {
		logging.Log.Warn().
			Err(err).
			Str("path", requestPath).
			Str("query", requestQuery).
			Str("origin", requestOrigin).
			Msg("Rejected websocket request due to authentication failure")
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	deviceID := auth.Session.GetClientId()
	if deviceID == "" {
		logging.Log.Warn().
			Str("path", requestPath).
			Str("accountId", auth.Account.GetId()).
			Str("origin", requestOrigin).
			Msg("Rejected websocket request due to missing client_id")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session does not contain client_id"})
		return
	}
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
					Str("accountId", auth.Account.GetId()).
					Str("deviceId", deviceID).
					Msg("Rejected websocket handshake due to invalid origin")
				return fmt.Errorf("invalid websocket origin: %w", err)
			}
			return nil
		},
		Handler: websocket.Handler(func(conn *websocket.Conn) {
			logging.Log.Info().
				Str("accountId", auth.Account.GetId()).
				Str("deviceId", deviceID).
				Str("origin", requestOrigin).
				Str("path", requestPath).
				Msg("Upgraded websocket connection")
			h.service.HandleConnection(c.Request.Context(), auth.Account, deviceID, conn)
		}),
	}

	server.ServeHTTP(c.Writer, c.Request)

	logging.Log.Debug().
		Str("accountId", auth.Account.GetId()).
		Str("deviceId", deviceID).
		Msg("Websocket handler completed")
}
