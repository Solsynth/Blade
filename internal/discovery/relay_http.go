package discovery

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

// maxRelayRegistrationBody bounds a registration payload.
const maxRelayRegistrationBody = 4 << 10

// RelayAPI is the relay control plane.
//
// Relays are deployed outside the cluster network, so they cannot reach the
// internal gRPC discovery service and the elected checker cannot dial them.
// They register, renew, and report their own health over the gateway's public
// HTTPS entry instead, which needs no new port and no public gRPC listener.
type RelayAPI struct {
	registry *Registry
	service  string
	token    string
	lease    time.Duration
}

func NewRelayAPI(registry *Registry, service, token string, lease time.Duration) *RelayAPI {
	return &RelayAPI{
		registry: registry,
		service:  service,
		token:    strings.TrimSpace(token),
		lease:    lease,
	}
}

// RegisterRoutes mounts the control endpoints on r.
//
// Mount it on a router without the readiness gate: a relay must be able to
// register and renew while the core fleet is unhealthy, which is exactly when
// the catalog has to stay accurate.
func (a *RelayAPI) RegisterRoutes(r gin.IRouter) {
	r.PUT("/relays/:id", a.upsert)
	r.DELETE("/relays/:id", a.deregister)
}

// relayRegistration is the self-reported state of one relay node.
type relayRegistration struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
	Region   string `json:"region"`
	Weight   int32  `json:"weight"`
	Healthy  bool   `json:"healthy"`
}

// upsert registers a relay or renews its lease, carrying the relay's own health
// report. It is idempotent so a relay can recover from a gateway restart by
// re-registering on its next heartbeat.
func (a *RelayAPI) upsert(c *gin.Context) {
	if !a.authorize(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay id is required"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayRegistrationBody)
	var body relayRegistration
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid relay registration payload"})
		return
	}

	host := strings.TrimSpace(body.Endpoint)
	if host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay endpoint is required"})
		return
	}
	if body.Port < 1 || body.Port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay port must be between 1 and 65535"})
		return
	}
	weight := body.Weight
	if weight < 1 {
		weight = 1
	}

	instance := &gen.DyServiceInstance{
		Service:    a.service,
		InstanceId: id,
		Weight:     weight,
		Endpoints: map[string]string{
			"tcp": net.JoinHostPort(host, strconv.Itoa(body.Port)),
		},
	}
	if region := strings.ToLower(strings.TrimSpace(body.Region)); region != "" {
		instance.Metadata = map[string]string{"region": region}
	}

	_, expiresAt, err := a.registry.RegisterSelfReported(
		c.Request.Context(),
		instance,
		a.lease,
		body.Healthy,
	)
	if err != nil {
		logging.Log.Warn().Err(err).Str("relay", id).Msg("Relay registration failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to register relay"})
		return
	}

	logging.Log.Info().
		Str("relay", id).
		Str("tcp", instance.GetEndpoints()["tcp"]).
		Str("region", instance.GetMetadata()["region"]).
		Bool("healthy", body.Healthy).
		Time("expiresAt", expiresAt).
		Msg("Relay registered")
	c.JSON(http.StatusOK, gin.H{
		"id":                       id,
		"lease_expires_at_unix_ms": expiresAt.UnixMilli(),
	})
}

// deregister withdraws a relay so it leaves the catalog before its lease ends.
func (a *RelayAPI) deregister(c *gin.Context) {
	if !a.authorize(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay id is required"})
		return
	}

	if err := a.registry.Deregister(c.Request.Context(), a.service, id); err != nil {
		logging.Log.Warn().Err(err).Str("relay", id).Msg("Relay deregistration failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to deregister relay"})
		return
	}

	logging.Log.Info().Str("relay", id).Msg("Relay deregistered")
	c.Status(http.StatusNoContent)
}

func (a *RelayAPI) authorize(c *gin.Context) bool {
	if a.token == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "relay registration is not configured"})
		return false
	}
	if !BearerMatches(c.GetHeader("Authorization"), a.token) {
		c.Header("WWW-Authenticate", "Bearer")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid relay credential"})
		return false
	}
	return true
}
