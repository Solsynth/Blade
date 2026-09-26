package discovery

import (
	"context"
	"reflect"
	"testing"
	"time"

	gen "src.solsynth.dev/sosys/go/proto"
)

func TestCatalog_ListRelaysWithTCPEndpoint(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	ctx := context.Background()

	register := func(instance *gen.DyServiceInstance) {
		t.Helper()
		if _, _, err := registry.Register(ctx, instance, time.Minute); err != nil {
			t.Fatalf("Register(%s) error = %v", instance.GetInstanceId(), err)
		}
	}
	register(&gen.DyServiceInstance{
		Service:    "relay",
		InstanceId: "jp-01",
		Endpoints: map[string]string{
			"tcp":  "relay-jp.solian.app:443",
			"http": "http://relay-jp.solian.app:8081",
		},
		Metadata: map[string]string{"region": "jp"},
		Weight:   2,
	})
	if err := registry.SetHealth(ctx, "relay", "jp-01", true); err != nil {
		t.Fatalf("SetHealth() error = %v", err)
	}
	// An instance without a tcp endpoint is not a dialable relay.
	register(&gen.DyServiceInstance{
		Service:      "relay",
		InstanceId:   "jp-02",
		GrpcEndpoint: "relay-jp-2.solian.app:9090",
	})

	catalog := NewCatalog(registry, "relay")
	relays, err := catalog.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []Relay{{ID: "jp-01", Endpoint: "relay-jp.solian.app", Port: 443, Region: "jp", Weight: 2, Healthy: true}}
	if !reflect.DeepEqual(relays, want) {
		t.Fatalf("List() = %+v, want %+v", relays, want)
	}
}

func TestCatalog_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	ctx := context.Background()
	// The default service name applies when discovery.relayServiceName is empty.
	catalog := NewCatalog(NewRegistry(nil, "test:discovery", time.Minute), "  ")

	relays, err := catalog.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if relays == nil || len(relays) != 0 {
		t.Fatalf("List() = %#v, want an empty non-nil slice", relays)
	}
}

func TestCatalog_SortsRelaysByIDAndSkipsBrokenEndpoints(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	ctx := context.Background()
	for _, instance := range []*gen.DyServiceInstance{
		{Service: "relay", InstanceId: "sg-01", Endpoints: map[string]string{"tcp": "relay-sg.solian.app:443"}},
		{Service: "relay", InstanceId: "jp-01", Endpoints: map[string]string{"tcp": "relay-jp.solian.app:443"}},
		{Service: "relay", InstanceId: "bad-01", Endpoints: map[string]string{"tcp": "relay-bad.solian.app"}},
		{Service: "relay", InstanceId: "bad-02", Endpoints: map[string]string{"tcp": "relay-bad.solian.app:not-a-port"}},
	} {
		if _, _, err := registry.Register(ctx, instance, time.Minute); err != nil {
			t.Fatalf("Register(%s) error = %v", instance.GetInstanceId(), err)
		}
	}

	relays, err := NewCatalog(registry, "relay").List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(relays) != 2 || relays[0].ID != "jp-01" || relays[1].ID != "sg-01" {
		t.Fatalf("List() = %+v, want jp-01 then sg-01", relays)
	}
}
