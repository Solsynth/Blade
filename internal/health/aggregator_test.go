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

func TestAggregatorMirrorsSelfReportedRelaysWithoutProbing(t *testing.T) {
	probes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	registry := discovery.NewRegistry(nil, "health-test", time.Minute)
	// A relay that reports itself healthy but publishes a health endpoint the
	// gateway could not reach from a different network.
	instance := &gen.DyServiceInstance{
		Service:    "relay",
		InstanceId: "jp-01",
		Endpoints: map[string]string{
			"tcp":  "relay-jp.solian.app:443",
			"http": server.URL,
		},
	}
	if _, _, err := registry.RegisterSelfReported(t.Context(), instance, time.Minute, true); err != nil {
		t.Fatalf("RegisterSelfReported() error = %v", err)
	}

	store := NewReadinessStore(nil)
	cfg := &config.Config{
		Health:    config.HealthConfig{CheckTimeout: time.Second},
		Discovery: config.DiscoveryConfig{RelayServiceName: "relay"},
	}
	aggregator := NewAggregator(store, cfg, registry)
	aggregator.tick(t.Context())

	if probes != 0 {
		t.Fatalf("probes = %d, want relays to be skipped by the checker", probes)
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if !instances[0].GetHealthy() {
		t.Fatal("the self-reported health must survive a check tick")
	}

	state, ok := store.GetServiceState("relay")
	if !ok || !state.IsHealthy {
		t.Fatalf("relay state = %+v, %v; want the reported health mirrored", state, ok)
	}
}

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
