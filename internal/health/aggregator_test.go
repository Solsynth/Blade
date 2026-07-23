package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/config"
	"srv.solsynth.dev/sosys/blade/internal/discovery"
)

func TestAggregatorChecksServicesDiscoveredOnlyThroughRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	registry := discovery.NewRegistry(nil, "health-test", time.Minute)
	if _, _, err := registry.Register(t.Context(), &gen.DyServiceInstance{
		Service: "dynamic", InstanceId: "dynamic-1", Endpoints: map[string]string{"http": server.URL},
	}, time.Minute); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	store := NewReadinessStore(nil)
	aggregator := NewAggregator(store, &config.Config{Health: config.HealthConfig{CheckTimeout: time.Second}}, registry)
	aggregator.tick(t.Context())

	state, ok := store.GetServiceState("dynamic")
	if !ok || !state.IsHealthy {
		t.Fatalf("registered-only service state = %+v, %v; want healthy state", state, ok)
	}
	instances, err := registry.List(t.Context(), "dynamic")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if !instances[0].GetHealthy() {
		t.Fatal("expected health probe to update the registered instance")
	}
}
