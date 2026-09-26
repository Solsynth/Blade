package discovery

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"

	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

// defaultRelayService is the registry service name relays register under.
const defaultRelayService = "relay"

// Relay is one L4 relay node as exposed to clients. Endpoint is the host only;
// clients add Port themselves.
type Relay struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
	Region   string `json:"region"`
	Weight   int32  `json:"weight"`
	Healthy  bool   `json:"healthy"`
}

// Catalog projects the registry's relay instances into the shape clients
// consume at GET /relays.
type Catalog struct {
	registry    *Registry
	serviceName string
}

// NewCatalog builds a catalog over the shared registry. An empty service name
// falls back to "relay".
func NewCatalog(registry *Registry, serviceName string) *Catalog {
	if strings.TrimSpace(serviceName) == "" {
		serviceName = defaultRelayService
	}
	return &Catalog{registry: registry, serviceName: strings.ToLower(strings.TrimSpace(serviceName))}
}

// List returns every registered relay, sorted by id, skipping instances whose
// tcp endpoint is missing or unparsable. The result is never nil.
func (c *Catalog) List(ctx context.Context) ([]Relay, error) {
	relays := make([]Relay, 0)
	if c == nil || c.registry == nil {
		return relays, nil
	}

	instances, err := c.registry.List(ctx, c.serviceName)
	if err != nil {
		return nil, err
	}

	relays = make([]Relay, 0, len(instances))
	for _, instance := range instances {
		host, port, ok := relayAddress(instance)
		if !ok {
			logging.Log.Debug().
				Str("service", c.serviceName).
				Str("instance", instance.GetInstanceId()).
				Str("tcp", Endpoint(instance, "tcp")).
				Msg("Skipping relay instance without a usable tcp endpoint")
			continue
		}
		relays = append(relays, Relay{
			ID:       instance.GetInstanceId(),
			Endpoint: host,
			Port:     port,
			Region:   instance.GetMetadata()["region"],
			Weight:   instance.GetWeight(),
			Healthy:  instance.GetHealthy(),
		})
	}

	sort.Slice(relays, func(i, j int) bool { return relays[i].ID < relays[j].ID })
	return relays, nil
}

// relayAddress parses the instance's tcp endpoint into host and port.
func relayAddress(instance *gen.DyServiceInstance) (string, int, bool) {
	endpoint := Endpoint(instance, "tcp")
	if endpoint == "" {
		return "", 0, false
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return "", 0, false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", 0, false
	}
	return host, number, true
}
