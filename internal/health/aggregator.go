package health

import (
	"context"
	"net/http"
	"time"

	"git.solsynth.dev/sosys/blade/internal/config"
	"git.solsynth.dev/sosys/blade/internal/logging"
	"github.com/gin-gonic/gin"
)

type Aggregator struct {
	store         *ReadinessStore
	services      map[string]string
	checkInterval time.Duration
	checkTimeout  time.Duration
}

func NewAggregator(store *ReadinessStore, cfg *config.Config) *Aggregator {
	services := make(map[string]string)
	for _, name := range cfg.Endpoints.ServiceNames {
		url := config.GetServiceHttp(name)
		if url != "" {
			services[name] = url
		}
	}

	return &Aggregator{
		store:         store,
		services:      services,
		checkInterval: time.Duration(cfg.Health.CheckIntervalSeconds) * time.Second,
		checkTimeout:  cfg.Health.CheckTimeout,
	}
}

func (a *Aggregator) Start(ctx context.Context) {
	ticker := time.NewTicker(a.checkInterval)
	defer ticker.Stop()

	a.checkAllServices(ctx)

	for {
		select {
		case <-ctx.Done():
			logging.Log.Info().Msg("Health aggregator stopping")
			return
		case <-ticker.C:
			a.checkAllServices(ctx)
		}
	}
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

func (a *Aggregator) checkService(ctx context.Context, name, baseURL string) {
	url := baseURL + "/health"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.store.UpdateService(ServiceState{
			ServiceName: name,
			IsHealthy:   false,
			LastChecked: time.Now(),
		})
		logging.Log.Warn().Str("service", name).Err(err).Msg("Failed to create health check request")
		return
	}

	client := &http.Client{Timeout: a.checkTimeout}
	resp, err := client.Do(req)
	if err != nil {
		a.store.UpdateService(ServiceState{
			ServiceName: name,
			IsHealthy:   false,
			LastChecked: time.Now(),
		})
		logging.Log.Warn().Str("service", name).Str("url", url).Err(err).Msg("Health check failed")
		return
	}
	defer resp.Body.Close()

	healthy := resp.StatusCode >= 200 && resp.StatusCode < 300
	a.store.UpdateService(ServiceState{
		ServiceName: name,
		IsHealthy:   healthy,
		LastChecked: time.Now(),
	})

	if healthy {
		logging.Log.Debug().Str("service", name).Int("status", resp.StatusCode).Msg("Service healthy")
	} else {
		logging.Log.Warn().Str("service", name).Int("status", resp.StatusCode).Msg("Service unhealthy")
	}
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
