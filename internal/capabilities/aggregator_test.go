package capabilities

import (
	"context"
	"encoding/json"
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
			return &gen.DyCapabilitiesResponse{ApiRevision: 17, MinimumRevision: 16, Capabilities: []*gen.DyCapabilityState{{Name: "drive.uploads", Enabled: true, Revision: 17, Version: "2"}}}, nil
		case "ring:5001":
			return &gen.DyCapabilitiesResponse{ApiRevision: 18, MinimumRevision: 17, Capabilities: []*gen.DyCapabilityState{{Capability: gen.DyCapability_DY_CAPABILITY_VOICE, Enabled: true, Revision: 18}}}, nil
		default:
			return nil, errors.New("unexpected endpoint")
		}
	})

	aggregator.Refresh(t.Context())
	document := aggregator.Document()
	if document.Incomplete {
		t.Fatalf("expected complete document, got %+v", document)
	}
	if document.APIRevision != 18 || document.MinimumRevision != 17 {
		t.Fatalf("unexpected revisions: %+v", document)
	}
	if !document.Features["voice"] || !document.Features["drive.uploads"] {
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
	document := aggregator.Document()
	if !document.Incomplete || document.Services["drive"].State != "degraded" {
		t.Fatalf("expected degraded service, got %+v", aggregator.Document())
	}
}

func TestDocumentIsInitiallyIncomplete(t *testing.T) {
	aggregator := NewWithFetch(testSource{}, func(context.Context, string) (*gen.DyCapabilitiesResponse, error) {
		return nil, nil
	})
	if !aggregator.Document().Incomplete {
		t.Fatal("expected initial document to be incomplete")
	}
}

func TestDocumentUsesSnakeCaseJSON(t *testing.T) {
	payload, err := json.Marshal(Document{
		APIRevision:     17,
		MinimumRevision: 16,
		Services: map[string]ServiceMetadata{
			"ring": {APIRevision: 17, MinimumRevision: 16},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := document["api_revision"]; !ok {
		t.Fatalf("expected api_revision in %s", payload)
	}
	if _, ok := document["apiRevision"]; ok {
		t.Fatalf("unexpected camel-case API revision in %s", payload)
	}
	if _, ok := document["incomplete"]; !ok {
		t.Fatalf("expected incomplete in %s", payload)
	}
}
