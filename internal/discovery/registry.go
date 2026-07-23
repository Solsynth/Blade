package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	gen "src.solsynth.dev/sosys/go/proto"
)

// Registry is the shared instance source of truth. Redis key expiry is the
// authoritative lease; the sorted-set index makes resolving a service cheap.
type Registry struct {
	client *redis.Client
	prefix string
	ttl    time.Duration

	mu        sync.RWMutex
	instances map[string]registryInstance
	cacheMu   sync.RWMutex
	cache     map[string]cachedInstances
	cacheTTL  time.Duration
	rr        atomic.Uint64
}

type registryInstance struct {
	Instance  *gen.DyServiceInstance `json:"instance"`
	ExpiresAt int64                  `json:"expires_at"`
}

type cachedInstances struct {
	instances []*gen.DyServiceInstance
	expiresAt time.Time
}

func NewRegistry(client *redis.Client, prefix string, lease time.Duration) *Registry {
	if lease <= 0 {
		lease = 30 * time.Second
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), ":")
	if prefix == "" {
		prefix = "blade:discovery"
	}
	return &Registry{
		client:    client,
		prefix:    prefix,
		ttl:       lease,
		instances: make(map[string]registryInstance),
		cache:     make(map[string]cachedInstances),
		cacheTTL:  time.Second,
	}
}

func (r *Registry) DefaultLease() time.Duration { return r.ttl }

func (r *Registry) instanceKey(service, id string) string {
	return r.prefix + ":instance:" + service + ":" + id
}
func (r *Registry) serviceKey(service string) string { return r.prefix + ":service:" + service }
func (r *Registry) leaderKey() string                { return r.prefix + ":health-leader" }
func instanceMapKey(service, id string) string       { return service + "\x00" + id }

func normalizeInstance(instance *gen.DyServiceInstance) (*gen.DyServiceInstance, error) {
	if instance == nil {
		return nil, fmt.Errorf("instance is required")
	}
	copy := proto.Clone(instance).(*gen.DyServiceInstance)
	copy.Service = strings.ToLower(strings.TrimSpace(copy.GetService()))
	copy.InstanceId = strings.TrimSpace(copy.GetInstanceId())
	copy.HttpEndpoint = strings.TrimRight(strings.TrimSpace(copy.GetHttpEndpoint()), "/")
	copy.GrpcEndpoint = strings.TrimSpace(copy.GetGrpcEndpoint())
	endpoints := make(map[string]string, len(copy.GetEndpoints()))
	for protocol, endpoint := range copy.GetEndpoints() {
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		endpoint = strings.TrimSpace(endpoint)
		if protocol == "" || endpoint == "" {
			return nil, fmt.Errorf("endpoint protocol and value are required")
		}
		endpoints[protocol] = endpoint
	}
	if len(endpoints) > 0 {
		copy.Endpoints = endpoints
	}
	if copy.Service == "" || copy.InstanceId == "" {
		return nil, fmt.Errorf("service and instance_id are required")
	}
	if copy.HttpEndpoint == "" && copy.GrpcEndpoint == "" && len(copy.GetEndpoints()) == 0 {
		return nil, fmt.Errorf("an endpoint is required")
	}
	if httpEndpoint := Endpoint(copy, "http"); httpEndpoint != "" {
		parsed, err := url.ParseRequestURI(httpEndpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("http endpoint must be an absolute URL")
		}
	}
	if copy.Weight < 1 {
		copy.Weight = 1
	}
	return copy, nil
}

// Endpoint returns a registered endpoint by protocol. Map entries take
// precedence over the legacy http_endpoint and grpc_endpoint fields.
func Endpoint(instance *gen.DyServiceInstance, protocol string) string {
	if instance == nil {
		return ""
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	for name, endpoint := range instance.GetEndpoints() {
		if strings.EqualFold(strings.TrimSpace(name), protocol) {
			return strings.TrimSpace(endpoint)
		}
	}
	switch protocol {
	case "http":
		return strings.TrimSpace(instance.GetHttpEndpoint())
	case "grpc":
		return strings.TrimSpace(instance.GetGrpcEndpoint())
	default:
		return ""
	}
}

// Register writes a new lease. New instances remain unhealthy until the
// elected checker has successfully probed /health.
func (r *Registry) Register(ctx context.Context, instance *gen.DyServiceInstance, lease time.Duration) (*gen.DyServiceInstance, time.Time, error) {
	instance, err := normalizeInstance(instance)
	if err != nil {
		return nil, time.Time{}, err
	}
	if lease <= 0 {
		lease = r.ttl
	}
	instance.Healthy = false
	return r.persist(ctx, instance, lease)
}

func (r *Registry) persist(ctx context.Context, instance *gen.DyServiceInstance, lease time.Duration) (*gen.DyServiceInstance, time.Time, error) {
	expiresAt := time.Now().Add(lease)
	record := registryInstance{Instance: instance, ExpiresAt: expiresAt.UnixMilli()}
	if r.client == nil {
		r.mu.Lock()
		r.instances[instanceMapKey(instance.Service, instance.InstanceId)] = record
		r.mu.Unlock()
		return instance, expiresAt, nil
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, time.Time{}, err
	}
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, r.instanceKey(instance.Service, instance.InstanceId), payload, lease)
	pipe.ZAdd(ctx, r.serviceKey(instance.Service), redis.Z{Score: float64(record.ExpiresAt), Member: instance.InstanceId})
	pipe.Expire(ctx, r.serviceKey(instance.Service), 2*lease)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, time.Time{}, err
	}
	r.invalidate(serviceCacheKey(instance.Service))
	return instance, expiresAt, nil
}

func (r *Registry) Renew(ctx context.Context, service, instanceID string, lease time.Duration) (time.Time, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	instanceID = strings.TrimSpace(instanceID)
	if service == "" || instanceID == "" {
		return time.Time{}, fmt.Errorf("service and instance_id are required")
	}
	instances, err := r.List(ctx, service)
	if err != nil {
		return time.Time{}, err
	}
	for _, instance := range instances {
		if instance.GetInstanceId() == instanceID {
			_, expires, err := r.persist(ctx, instance, lease)
			return expires, err
		}
	}
	return time.Time{}, fmt.Errorf("service instance not found")
}

func (r *Registry) Deregister(ctx context.Context, service, instanceID string) error {
	service = strings.ToLower(strings.TrimSpace(service))
	instanceID = strings.TrimSpace(instanceID)
	if service == "" || instanceID == "" {
		return fmt.Errorf("service and instance_id are required")
	}
	if r.client == nil {
		r.mu.Lock()
		delete(r.instances, instanceMapKey(service, instanceID))
		r.mu.Unlock()
		return nil
	}
	pipe := r.client.TxPipeline()
	pipe.Del(ctx, r.instanceKey(service, instanceID))
	pipe.ZRem(ctx, r.serviceKey(service), instanceID)
	_, err := pipe.Exec(ctx)
	if err == nil {
		r.invalidate(serviceCacheKey(service))
	}
	return err
}

func (r *Registry) List(ctx context.Context, service string) ([]*gen.DyServiceInstance, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	if service == "" {
		return nil, fmt.Errorf("service is required")
	}
	if r.client == nil {
		now := time.Now().UnixMilli()
		r.mu.Lock()
		out := make([]*gen.DyServiceInstance, 0)
		for key, record := range r.instances {
			if record.ExpiresAt <= now {
				delete(r.instances, key)
				continue
			}
			if record.Instance.GetService() == service {
				out = append(out, record.Instance)
			}
		}
		r.mu.Unlock()
		sortInstances(out)
		return out, nil
	}
	if instances, ok := r.cached(service); ok {
		return instances, nil
	}
	now := time.Now().UnixMilli()
	if err := r.client.ZRemRangeByScore(ctx, r.serviceKey(service), "-inf", fmt.Sprintf("%d", now)).Err(); err != nil {
		return nil, err
	}
	ids, err := r.client.ZRangeByScore(ctx, r.serviceKey(service), &redis.ZRangeBy{Min: fmt.Sprintf("(%d", now), Max: "+inf"}).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = r.instanceKey(service, id)
	}
	values, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*gen.DyServiceInstance, 0, len(values))
	for i, value := range values {
		payload, ok := value.(string)
		if !ok {
			_ = r.client.ZRem(ctx, r.serviceKey(service), ids[i]).Err()
			continue
		}
		var record registryInstance
		if err := json.Unmarshal([]byte(payload), &record); err != nil || record.ExpiresAt <= now || record.Instance == nil {
			continue
		}
		out = append(out, record.Instance)
	}
	sortInstances(out)
	r.setCached(service, out)
	return out, nil
}

func (r *Registry) ListServices(ctx context.Context) ([]string, error) {
	if r.client == nil {
		now := time.Now().UnixMilli()
		services := make(map[string]struct{})
		r.mu.Lock()
		for key, record := range r.instances {
			if record.ExpiresAt <= now {
				delete(r.instances, key)
				continue
			}
			services[record.Instance.GetService()] = struct{}{}
		}
		r.mu.Unlock()
		return sortedServices(services), nil
	}

	services := make(map[string]struct{})
	pattern := r.prefix + ":service:*"
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			service := strings.TrimPrefix(key, r.prefix+":service:")
			if service != "" {
				services[service] = struct{}{}
			}
		}
		cursor = next
		if cursor == 0 {
			return sortedServices(services), nil
		}
	}
}

func sortedServices(services map[string]struct{}) []string {
	out := make([]string, 0, len(services))
	for service := range services {
		out = append(out, service)
	}
	sort.Strings(out)
	return out
}

func sortInstances(instances []*gen.DyServiceInstance) {
	sort.Slice(instances, func(i, j int) bool { return instances[i].GetInstanceId() < instances[j].GetInstanceId() })
}

func serviceCacheKey(service string) string { return strings.ToLower(strings.TrimSpace(service)) }

func (r *Registry) cached(service string) ([]*gen.DyServiceInstance, bool) {
	r.cacheMu.RLock()
	entry, ok := r.cache[serviceCacheKey(service)]
	r.cacheMu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return cloneInstances(entry.instances), true
}

func (r *Registry) setCached(service string, instances []*gen.DyServiceInstance) {
	r.cacheMu.Lock()
	r.cache[serviceCacheKey(service)] = cachedInstances{instances: cloneInstances(instances), expiresAt: time.Now().Add(r.cacheTTL)}
	r.cacheMu.Unlock()
}

func (r *Registry) invalidate(service string) {
	r.cacheMu.Lock()
	delete(r.cache, serviceCacheKey(service))
	r.cacheMu.Unlock()
}

func cloneInstances(instances []*gen.DyServiceInstance) []*gen.DyServiceInstance {
	clones := make([]*gen.DyServiceInstance, 0, len(instances))
	for _, instance := range instances {
		clones = append(clones, proto.Clone(instance).(*gen.DyServiceInstance))
	}
	return clones
}

func (r *Registry) SetHealth(ctx context.Context, service, instanceID string, healthy bool) error {
	instances, err := r.List(ctx, service)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.GetInstanceId() == instanceID {
			instance.Healthy = healthy
			_, _, err := r.persist(ctx, instance, r.remainingLease(ctx, service, instanceID))
			return err
		}
	}
	return fmt.Errorf("service instance not found")
}

func (r *Registry) remainingLease(ctx context.Context, service, instanceID string) time.Duration {
	if r.client == nil {
		return r.ttl
	}
	ttl, err := r.client.TTL(ctx, r.instanceKey(strings.ToLower(service), instanceID)).Result()
	if err != nil || ttl <= 0 {
		return r.ttl
	}
	return ttl
}

func (r *Registry) ResolveHTTP(ctx context.Context, service string) (string, bool) {
	instances, err := r.List(ctx, service)
	if err != nil {
		return "", false
	}
	available := make([]*gen.DyServiceInstance, 0, len(instances))
	for _, instance := range instances {
		if instance.GetHealthy() && Endpoint(instance, "http") != "" {
			available = append(available, instance)
		}
	}
	if len(available) == 0 {
		return "", false
	}
	return Endpoint(available[r.rr.Add(1)%uint64(len(available))], "http"), true
}

func (r *Registry) AcquireHealthLeadership(ctx context.Context, holder string, ttl time.Duration) (bool, error) {
	if r.client == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	return r.client.SetNX(ctx, r.leaderKey(), holder, ttl).Result()
}

func (r *Registry) RenewHealthLeadership(ctx context.Context, holder string, ttl time.Duration) (bool, error) {
	if r.client == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	const script = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("PEXPIRE", KEYS[1], ARGV[2]) else return 0 end`
	result, err := r.client.Eval(ctx, script, []string{r.leaderKey()}, holder, ttl.Milliseconds()).Int()
	return result == 1, err
}
