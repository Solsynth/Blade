package discovery

import (
	"context"
	"testing"
	"time"

	gen "src.solsynth.dev/sosys/go/proto"
)

func TestRegistry_RegisteredInstanceRequiresHealthCheckBeforeResolution(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	ctx := context.Background()

	_, _, err := registry.Register(ctx, &gen.DyServiceInstance{
		Service:      "Sphere",
		InstanceId:   "sphere-1",
		HttpEndpoint: "http://sphere-1:8000",
	}, 0)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, ok := registry.ResolveHTTP(ctx, "sphere"); ok {
		t.Fatal("new instance must not be routable before a successful health check")
	}

	if err := registry.SetHealth(ctx, "sphere", "sphere-1", true); err != nil {
		t.Fatalf("SetHealth() error = %v", err)
	}
	endpoint, ok := registry.ResolveHTTP(ctx, "sphere")
	if !ok || endpoint != "http://sphere-1:8000" {
		t.Fatalf("ResolveHTTP() = %q, %v; want healthy instance", endpoint, ok)
	}
}

func TestRegistry_RenewPreservesHealth(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	ctx := context.Background()
	_, _, err := registry.Register(ctx, &gen.DyServiceInstance{
		Service:      "ring",
		InstanceId:   "ring-1",
		HttpEndpoint: "http://ring-1:8000",
	}, time.Second)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.SetHealth(ctx, "ring", "ring-1", true); err != nil {
		t.Fatalf("SetHealth() error = %v", err)
	}
	if _, err := registry.Renew(ctx, "ring", "ring-1", time.Minute); err != nil {
		t.Fatalf("Renew() error = %v", err)
	}
	if _, ok := registry.ResolveHTTP(ctx, "ring"); !ok {
		t.Fatal("renewing a healthy instance must keep it routable")
	}
}

func TestRegistry_EndpointMapOverridesLegacyEndpoints(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	ctx := context.Background()
	_, _, err := registry.Register(ctx, &gen.DyServiceInstance{
		Service: "sphere", InstanceId: "sphere-1",
		HttpEndpoint: "http://legacy:8000",
		Endpoints: map[string]string{
			"HTTP": "http://registered:8000",
			"nats": "nats://broker/sphere",
		},
	}, time.Minute)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.SetHealth(ctx, "sphere", "sphere-1", true); err != nil {
		t.Fatalf("SetHealth() error = %v", err)
	}
	endpoint, ok := registry.ResolveHTTP(ctx, "sphere")
	if !ok || endpoint != "http://registered:8000" {
		t.Fatalf("ResolveHTTP() = %q, %v; want map endpoint", endpoint, ok)
	}
	instances, err := registry.List(ctx, "sphere")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if Endpoint(instances[0], "nats") != "nats://broker/sphere" {
		t.Fatalf("custom endpoint = %q", Endpoint(instances[0], "nats"))
	}
}
