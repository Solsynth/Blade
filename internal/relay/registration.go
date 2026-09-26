package relay

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

const (
	registrationCallTimeout = 10 * time.Second
	registrationRetryDelay  = 5 * time.Second
	registrationRetryMax    = 30 * time.Second
	deregisterTimeout       = 5 * time.Second
	defaultHealthPort       = 8081
)

// Registration publishes this relay into Blade's service registry and keeps
// its lease renewed, mirroring the register -> renew -> deregister lifecycle
// the rest of the fleet uses.
type Registration struct {
	client   gen.DyServiceDiscoveryServiceClient
	cfg      Config
	instance *gen.DyServiceInstance

	mu         sync.Mutex
	registered bool
}

// NewRegistration builds the instance record that is published on every
// (re-)registration. cfg supplies the public address, region, weight, and the
// health endpoint Blade probes.
func NewRegistration(client gen.DyServiceDiscoveryServiceClient, cfg Config) *Registration {
	instanceMetadata := make(map[string]string, 1)
	if cfg.Relay.Region != "" {
		instanceMetadata["region"] = cfg.Relay.Region
	}
	return &Registration{
		client: client,
		cfg:    cfg,
		instance: &gen.DyServiceInstance{
			Service:    cfg.Discovery.Service,
			InstanceId: cfg.Relay.ID,
			Weight:     cfg.Relay.Weight,
			Endpoints: map[string]string{
				"tcp":  net.JoinHostPort(cfg.Relay.PublicHost, strconv.Itoa(cfg.Relay.PublicPort)),
				"http": healthAdvertise(cfg),
			},
			Metadata: instanceMetadata,
		},
	}
}

// healthAdvertise is the endpoint Blade probes. Operators should point it at an
// address reachable from Blade; otherwise it falls back to the public host and
// the health listener's port.
func healthAdvertise(cfg Config) string {
	if cfg.Health.Advertise != "" {
		return cfg.Health.Advertise
	}
	port := defaultHealthPort
	if _, parsed, err := net.SplitHostPort(cfg.Health.Listen); err == nil {
		if number, convErr := strconv.Atoi(parsed); convErr == nil {
			port = number
		}
	}
	return "http://" + net.JoinHostPort(cfg.Relay.PublicHost, strconv.Itoa(port))
}

// Run registers and renews until ctx is cancelled. Registration failures retry
// with exponential backoff (5s doubling to a 30s cap); a failed renewal falls
// back to a fresh registration slot.
func (r *Registration) Run(ctx context.Context) {
	retryDelay := registrationRetryDelay
	for {
		interval, err := r.register(ctx)
		if err == nil {
			r.setRegistered(true)
			retryDelay = registrationRetryDelay
			r.renewLoop(ctx, interval)
			if ctx.Err() != nil {
				return
			}
			// The lease was lost: fall through and register again.
		} else if ctx.Err() != nil {
			return
		} else {
			r.setRegistered(false)
			logging.Log.Warn().
				Err(err).
				Str("service", r.cfg.Discovery.Service).
				Str("instance", r.instance.GetInstanceId()).
				Str("target", r.cfg.Discovery.Target).
				Dur("retryIn", retryDelay).
				Msg("Relay service discovery registration failed")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
		if retryDelay *= 2; retryDelay > registrationRetryMax {
			retryDelay = registrationRetryMax
		}
	}
}

func (r *Registration) register(ctx context.Context) (time.Duration, error) {
	callCtx, cancel := context.WithTimeout(ctx, registrationCallTimeout)
	defer cancel()

	response, err := r.client.Register(r.authorized(callCtx), &gen.DyRegisterServiceInstanceRequest{
		Instance:     r.instance,
		LeaseSeconds: int32(r.cfg.Discovery.LeaseSeconds),
	})
	if err != nil {
		return 0, err
	}
	logging.Log.Info().
		Str("service", r.cfg.Discovery.Service).
		Str("instance", r.instance.GetInstanceId()).
		Str("tcp", r.instance.GetEndpoints()["tcp"]).
		Str("http", r.instance.GetEndpoints()["http"]).
		Msg("Relay registered with Blade service discovery")
	return RenewalInterval(response.GetLeaseExpiresAtUnixMs(), int32(r.cfg.Discovery.LeaseSeconds), time.Now()), nil
}

func (r *Registration) renewLoop(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		callCtx, cancel := context.WithTimeout(ctx, registrationCallTimeout)
		response, err := r.client.Renew(r.authorized(callCtx), &gen.DyRenewServiceLeaseRequest{
			Service:      r.cfg.Discovery.Service,
			InstanceId:   r.instance.GetInstanceId(),
			LeaseSeconds: int32(r.cfg.Discovery.LeaseSeconds),
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logging.Log.Warn().
				Err(err).
				Str("service", r.cfg.Discovery.Service).
				Str("instance", r.instance.GetInstanceId()).
				Msg("Relay service discovery renewal failed; re-registering")
			return
		}
		interval = RenewalInterval(response.GetLeaseExpiresAtUnixMs(), int32(r.cfg.Discovery.LeaseSeconds), time.Now())
	}
}

// Deregister removes this relay from the registry. It is best effort: the
// lease expires on its own if the call does not land.
func (r *Registration) Deregister(ctx context.Context) {
	if !r.isRegistered() {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, deregisterTimeout)
	defer cancel()

	if _, err := r.client.Deregister(r.authorized(callCtx), &gen.DyDeregisterServiceInstanceRequest{
		Service:    r.cfg.Discovery.Service,
		InstanceId: r.instance.GetInstanceId(),
	}); err != nil {
		logging.Log.Warn().
			Err(err).
			Str("service", r.cfg.Discovery.Service).
			Str("instance", r.instance.GetInstanceId()).
			Msg("Relay service discovery deregistration failed")
		return
	}
	r.setRegistered(false)
}

// RenewalInterval is a third of the remaining lease, never below one second.
// It is pure so the cadence can be unit-tested.
func RenewalInterval(leaseExpiresAtUnixMs int64, leaseSeconds int32, now time.Time) time.Duration {
	if leaseExpiresAtUnixMs > 0 {
		if remaining := time.UnixMilli(leaseExpiresAtUnixMs).Sub(now); remaining > 0 {
			return clampRenewal(remaining.Seconds() / 3)
		}
	}
	return clampRenewal(float64(leaseSeconds) / 3)
}

func clampRenewal(seconds float64) time.Duration {
	if seconds < 1 {
		return time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

func (r *Registration) authorized(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(r.cfg.Discovery.RegistrationToken))
}

func (r *Registration) setRegistered(registered bool) {
	r.mu.Lock()
	r.registered = registered
	r.mu.Unlock()
}

func (r *Registration) isRegistered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registered
}
