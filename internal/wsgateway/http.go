package wsgateway

import (
	"net/http"
	"time"

	"git.solsynth.dev/solarnetwork/blade/internal/logging"
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
	deviceAlt := c.Query("deviceAlt")
	if deviceAlt != "" {
		if _, ok := h.cfg.AllowedDeviceAlt[deviceAlt]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported deviceAlt"})
			return
		}
	}

	auth, err := authenticateRequest(c.Request.Context(), h.authenticator, c.Request)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	deviceID := auth.Session.GetClientId()
	if deviceID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session does not contain client_id"})
		return
	}
	if deviceAlt != "" {
		deviceID = deviceID + "+" + deviceAlt
	}

	websocket.Handler(func(conn *websocket.Conn) {
		h.service.HandleConnection(c.Request.Context(), auth.Account, deviceID, conn)
	}).ServeHTTP(c.Writer, c.Request)

	logging.Log.Debug().Str("accountId", auth.Account.GetId()).Str("deviceId", deviceID).Msg("Accepted websocket connection")
}
