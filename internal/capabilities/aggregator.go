package capabilities

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"
)

const (
	refreshInterval = 5 * time.Minute
	queryTimeout    = 5 * time.Second
)

type ServiceSource interface {
	ListServices(context.Context) ([]string, error)
	List(context.Context, string) ([]*gen.DyServiceInstance, error)
}

type CapabilityState struct {
	Enabled      bool   `json:"enabled"`
	Revision     uint32 `json:"revision,omitempty"`
	Experimental bool   `json:"experimental,omitempty"`
	Version      string `json:"version,omitempty"`
}

type ServiceMetadata struct {
	APIRevision     uint32                     `json:"apiRevision"`
	MinimumRevision uint32                     `json:"minimumRevision"`
	Capabilities    map[string]CapabilityState `json:"capabilities"`
	State           string                     `json:"state"`
}

type Document struct {
	APIRevision     uint32                     `json:"apiRevision"`
	MinimumRevision uint32                     `json:"minimumRevision"`
	Features        map[string]bool            `json:"features"`
	Capabilities    map[string]CapabilityState `json:"capabilities"`
	Services        map[string]ServiceMetadata `json:"services"`
}

type Fetch func(context.Context, string) (*gen.DyCapabilitiesResponse, error)

type Aggregator struct {
	source ServiceSource
	fetch  Fetch

	mu       sync.RWMutex
	document Document
}

func New(source ServiceSource) *Aggregator {
	return NewWithFetch(source, fetchCapabilities)
}

func NewWithFetch(source ServiceSource, fetch Fetch) *Aggregator {
	return &Aggregator{source: source, fetch: fetch, document: emptyDocument()}
}

func (a *Aggregator) Start(ctx context.Context) {
	a.Refresh(ctx)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Refresh(ctx)
		}
	}
}

func (a *Aggregator) Refresh(ctx context.Context) {
	services, err := a.source.ListServices(ctx)
	if err != nil {
		return
	}

	document := emptyDocument()
	for _, service := range services {
		metadata := ServiceMetadata{Capabilities: make(map[string]CapabilityState), State: "degraded"}
		document.Services[service] = metadata

		instances, err := a.source.List(ctx, service)
		if err != nil {
			continue
		}
		for _, instance := range instances {
			if !instance.GetHealthy() || instance.GetGrpcEndpoint() == "" {
				continue
			}
			response, err := a.fetch(ctx, instance.GetGrpcEndpoint())
			if err != nil {
				continue
			}
			metadata = responseMetadata(response)
			metadata.State = "up"
			document.Services[service] = metadata
			merge(&document, metadata)
			break
		}
	}

	a.mu.Lock()
	a.document = document
	a.mu.Unlock()
}

func (a *Aggregator) Document() Document {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneDocument(a.document)
}

func fetchCapabilities(ctx context.Context, endpoint string) (*gen.DyCapabilitiesResponse, error) {
	target := endpoint
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		target = parsed.Host
	}
	callCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := grpc.DialContext(callCtx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return gen.NewDyCapabilitiesServiceClient(conn).GetCapabilities(callCtx, &emptypb.Empty{})
}

func emptyDocument() Document {
	return Document{
		Features:     make(map[string]bool),
		Capabilities: make(map[string]CapabilityState),
		Services:     make(map[string]ServiceMetadata),
	}
}

func responseMetadata(response *gen.DyCapabilitiesResponse) ServiceMetadata {
	metadata := ServiceMetadata{
		APIRevision:     response.GetApiRevision(),
		MinimumRevision: response.GetMinimumRevision(),
		Capabilities:    make(map[string]CapabilityState),
	}
	for _, state := range response.GetCapabilities() {
		name, ok := capabilityName(state.GetCapability())
		if !ok {
			continue
		}
		metadata.Capabilities[name] = CapabilityState{
			Enabled:      state.GetEnabled(),
			Revision:     state.GetRevision(),
			Experimental: state.GetExperimental(),
			Version:      state.GetVersion(),
		}
	}
	return metadata
}

func merge(document *Document, metadata ServiceMetadata) {
	document.APIRevision = max(document.APIRevision, metadata.APIRevision)
	document.MinimumRevision = max(document.MinimumRevision, metadata.MinimumRevision)
	for name, state := range metadata.Capabilities {
		if _, exists := document.Capabilities[name]; !exists || state.Enabled {
			document.Capabilities[name] = state
		}
		document.Features[name] = document.Features[name] || state.Enabled
	}
}

func capabilityName(capability gen.DyCapability) (string, bool) {
	name, ok := gen.DyCapability_name[int32(capability)]
	if !ok || capability == gen.DyCapability_DY_CAPABILITY_UNSPECIFIED {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, "DY_CAPABILITY_"), "_", "-")), true
}

func cloneDocument(document Document) Document {
	clone := emptyDocument()
	clone.APIRevision = document.APIRevision
	clone.MinimumRevision = document.MinimumRevision
	for name, enabled := range document.Features {
		clone.Features[name] = enabled
	}
	for name, state := range document.Capabilities {
		clone.Capabilities[name] = state
	}
	for name, service := range document.Services {
		capabilities := make(map[string]CapabilityState, len(service.Capabilities))
		for capability, state := range service.Capabilities {
			capabilities[capability] = state
		}
		service.Capabilities = capabilities
		clone.Services[name] = service
	}
	return clone
}

func SortedFeatureNames(document Document) []string {
	names := make([]string, 0, len(document.Features))
	for name := range document.Features {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
