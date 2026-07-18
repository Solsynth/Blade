package capabilities

import (
	"context"
	"errors"
	"testing"

	gen "src.solsynth.dev/sosys/go/proto"
)

type testSource struct {
	instances map[string][]*gen.DyServiceInstance
}

func (s testSource) ListServices(context.Context) ([]string, error) {
	return []string{"drive", "ring"}, nil
}

func (s testSource) List(_ context.Context, service string) ([]*gen.DyServiceInstance, error) {
	return s.instances[service], nil
}

func TestAggregatorRefreshAggregatesHealthyServices(t *testing.T) {
	aggregator := NewWithFetch(testSource{instances: map[string][]*gen.DyServiceInstance{
		"drive": {{Service: "drive", InstanceId: "drive-1", GrpcEndpoint: "drive:5001", Healthy: true}},
		"ring":  {{Service: "ring", InstanceId: "ring-1", GrpcEndpoint: "ring:5001", Healthy: true}},
	}}, func(_ context.Context, endpoint string) (*gen.DyCapabilitiesResponse, error) {
		switch endpoint {
		case "drive:5001":
			return &gen.DyCapabilitiesResponse{ApiRevision: 17, MinimumRevision: 16, Capabilities: []*gen.DyCapabilityState{{Capability: gen.DyCapability_DY_CAPABILITY_DRIVE_RESUMABLE, Enabled: true, Revision: 17, Version: "2"}}}, nil
		case "ring:5001":
			return &gen.DyCapabilitiesResponse{ApiRevision: 18, MinimumRevision: 17, Capabilities: []*gen.DyCapabilityState{{Capability: gen.DyCapability_DY_CAPABILITY_VOICE, Enabled: true, Revision: 18}}}, nil
		default:
			return nil, errors.New("unexpected endpoint")
		}
	})

	aggregator.Refresh(t.Context())
	document := aggregator.Document()
	if document.APIRevision != 18 || document.MinimumRevision != 17 {
		t.Fatalf("unexpected revisions: %+v", document)
	}
	if !document.Features["voice"] || !document.Features["drive-resumable"] {
		t.Fatalf("expected aggregated features, got %+v", document.Features)
	}
	if document.Services["drive"].State != "up" || document.Services["ring"].State != "up" {
		t.Fatalf("expected healthy service metadata, got %+v", document.Services)
	}
}

func TestAggregatorRefreshSkipsUnhealthyInstances(t *testing.T) {
	aggregator := NewWithFetch(testSource{instances: map[string][]*gen.DyServiceInstance{
		"drive": {{Service: "drive", InstanceId: "drive-1", GrpcEndpoint: "drive:5001", Healthy: false}},
	}}, func(context.Context, string) (*gen.DyCapabilitiesResponse, error) {
		t.Fatal("fetch must not be called for unhealthy instances")
		return nil, nil
	})

	aggregator.Refresh(t.Context())
	if aggregator.Document().Services["drive"].State != "degraded" {
		t.Fatalf("expected degraded service, got %+v", aggregator.Document())
	}
}
