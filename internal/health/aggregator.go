package health

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"srv.solsynth.dev/sosys/blade/internal/config"
	discovery "srv.solsynth.dev/sosys/blade/internal/discovery"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

type Aggregator struct {
	store         *ReadinessStore
	services      map[string]string
	checkInterval time.Duration
	checkTimeout  time.Duration
	registry      *discovery.Registry
	leaderID      string
	leaderLease   time.Duration
}

func NewAggregator(store *ReadinessStore, cfg *config.Config, registries ...*discovery.Registry) *Aggregator {
	services := make(map[string]string)
	for _, name := range cfg.Endpoints.ServiceNames {
		url := config.GetServiceHttp(name)
		if url != "" {
			services[name] = url
		}
	}

	a := &Aggregator{
		store:         store,
		services:      services,
		checkInterval: time.Duration(cfg.Health.CheckIntervalSeconds) * time.Second,
		checkTimeout:  cfg.Health.CheckTimeout,
		leaderID:      uuid.NewString(),
		leaderLease:   time.Duration(cfg.Discovery.LeaderLeaseSeconds) * time.Second,
	}
	if len(registries) > 0 {
		a.registry = registries[0]
	}
	return a
}

func (a *Aggregator) Start(ctx context.Context) {
	ticker := time.NewTicker(a.checkInterval)
	defer ticker.Stop()

	a.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			logging.Log.Info().Msg("Health aggregator stopping")
			return
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

func (a *Aggregator) tick(ctx context.Context) {
	if a.registry == nil {
		a.checkAllServices(ctx)
		return
	}
	services := a.effectiveServices(ctx)
	leader, err := a.registry.RenewHealthLeadership(ctx, a.leaderID, a.leaderLease)
	if err != nil {
		logging.Log.Warn().Err(err).Msg("Unable to renew discovery health leadership")
	}
	if !leader {
		leader, err = a.registry.AcquireHealthLeadership(ctx, a.leaderID, a.leaderLease)
		if err != nil {
			logging.Log.Warn().Err(err).Msg("Unable to acquire discovery health leadership")
		}
	}
	if leader {
		a.checkRegisteredServices(ctx, services)
	}
	a.syncRegisteredReadiness(ctx, services)
}

func (a *Aggregator) checkAllServices(ctx context.Context) {
	for name, baseURL := range a.services {
		select {
		case <-ctx.Done():
			return
		default:
			a.checkService(ctx, name, baseURL)
		}
	}
}

// effectiveServices combines configured services with the discovery registry.
// A registered service owns its runtime endpoint and health state; a configured
// URL is used only when that service has no registered instances.
func (a *Aggregator) effectiveServices(ctx context.Context) map[string]string {
	services := make(map[string]string, len(a.services))
	for name, endpoint := range a.services {
		services[name] = endpoint
	}

	registered, err := a.registry.ListServices(ctx)
	if err != nil {
		logging.Log.Warn().Err(err).Msg("Unable to list registered services")
		return services
	}
	for _, name := range registered {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, configured := services[name]; !configured {
			services[name] = ""
		}
	}
	return services
}

func (a *Aggregator) checkRegisteredServices(ctx context.Context, services map[string]string) {
	for name, fallbackURL := range services {
		instances, err := a.registry.List(ctx, name)
		if err != nil {
			logging.Log.Warn().Str("service", name).Err(err).Msg("Unable to list registered instances")
			continue
		}
		if len(instances) == 0 {
			if fallbackURL != "" {
				a.checkService(ctx, name, fallbackURL)
			} else {
				a.store.UpdateService(ServiceState{ServiceName: name, IsHealthy: false, LastChecked: time.Now()})
			}
			continue
		}
		for _, instance := range instances {
			httpEndpoint := discovery.Endpoint(instance, "http")
			if httpEndpoint == "" {
				continue
			}
			healthy := a.probe(ctx, name, httpEndpoint)
			if err := a.registry.SetHealth(ctx, name, instance.GetInstanceId(), healthy); err != nil {
				logging.Log.Warn().Str("service", name).Str("instance", instance.GetInstanceId()).Err(err).Msg("Unable to store instance health")
			}
		}
	}
}

func (a *Aggregator) syncRegisteredReadiness(ctx context.Context, services map[string]string) {
	for name, fallbackURL := range services {
		instances, err := a.registry.List(ctx, name)
		if err != nil || len(instances) == 0 {
			// Static endpoints are a temporary migration fallback. They are not
			// represented in Redis, so every replica retains the old local check.
			if fallbackURL != "" {
				a.checkService(ctx, name, fallbackURL)
			} else {
				a.store.UpdateService(ServiceState{ServiceName: name, IsHealthy: false, LastChecked: time.Now()})
			}
			continue
		}
		healthy := false
		for _, instance := range instances {
			if instance.GetHealthy() {
				healthy = true
				break
			}
		}
		a.store.UpdateService(ServiceState{ServiceName: name, IsHealthy: healthy, LastChecked: time.Now()})
	}
}

func (a *Aggregator) checkService(ctx context.Context, name, baseURL string) {
	healthy := a.probe(ctx, name, baseURL)
	a.store.UpdateService(ServiceState{ServiceName: name, IsHealthy: healthy, LastChecked: time.Now()})
}

func (a *Aggregator) probe(ctx context.Context, name, baseURL string) bool {
	url := strings.TrimRight(baseURL, "/") + "/health"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logging.Log.Warn().Str("service", name).Err(err).Msg("Failed to create health check request")
		return false
	}

	client := &http.Client{Timeout: a.checkTimeout}
	resp, err := client.Do(req)
	if err != nil {
		logging.Log.Warn().Str("service", name).Str("url", url).Err(err).Msg("Health check failed")
		return false
	}
	defer resp.Body.Close()

	healthy := resp.StatusCode >= 200 && resp.StatusCode < 300
	if healthy {
		logging.Log.Debug().Str("service", name).Int("status", resp.StatusCode).Msg("Service healthy")
	} else {
		logging.Log.Warn().Str("service", name).Int("status", resp.StatusCode).Msg("Service unhealthy")
	}
	return healthy
}

func (a *Aggregator) GetStore() *ReadinessStore {
	return a.store
}

func ReadinessMiddleware(store *ReadinessStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !store.IsCoreServiceHealthy() {
			c.Header("X-NotReady", "true")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "service not ready",
				"code":  "SERVICE_NOT_READY",
			})
			return
		}
		c.Next()
	}
}
