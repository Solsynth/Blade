package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"srv.solsynth.dev/sosys/blade/internal/logging"
)

const (
	registrationCallTimeout = 10 * time.Second
	registrationRetryDelay  = 5 * time.Second
	registrationRetryMax    = 30 * time.Second
	deregisterTimeout       = 5 * time.Second
	defaultRenewalInterval  = 10 * time.Second
	maxRegistrationBody     = 4 << 10
)

// HealthFunc reports the upstreams this relay cannot reach right now, keyed by
// target. An empty map means every upstream answered.
type HealthFunc func(ctx context.Context) map[string]string

// Registration publishes this relay into the gateway's relay catalog and keeps
// its lease renewed.
//
// Relays are deployed outside the cluster network: they cannot reach the
// internal gRPC discovery service and the gateway cannot dial them, so the
// control plane runs over the public HTTPS entry and health is reported here
// instead of being probed. A relay that stops reporting simply expires.
type Registration struct {
	cfg      Config
	client   *http.Client
	endpoint string
	health   HealthFunc

	mu         sync.Mutex
	registered bool
	reported   *bool
}

// NewRegistration prepares the control client. health supplies the state sent
// on every heartbeat; pass nil to report healthy unconditionally.
func NewRegistration(cfg Config, health HealthFunc) (*Registration, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.Discovery.URL))
	if err != nil {
		return nil, fmt.Errorf("discovery.url: %w", err)
	}
	if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("discovery.url must be an absolute http(s) URL, got %q", cfg.Discovery.URL)
	}

	client := &http.Client{Timeout: registrationCallTimeout}
	if cfg.Discovery.TLSSkipVerify {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.Transport = transport
	}

	control := *base
	control.Path = strings.TrimRight(base.Path, "/") + "/relays/" + url.PathEscape(cfg.Relay.ID)

	return &Registration{
		cfg:      cfg,
		client:   client,
		endpoint: control.String(),
		health:   health,
	}, nil
}

// relayRegistration is the payload the gateway stores for this relay.
type relayRegistration struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
	Region   string `json:"region"`
	Weight   int32  `json:"weight"`
	Healthy  bool   `json:"healthy"`
}

type relayRegistrationResponse struct {
	ID                   string `json:"id"`
	LeaseExpiresAtUnixMs int64  `json:"lease_expires_at_unix_ms"`
}

// Run publishes and renews until ctx is cancelled. Failures retry with
// exponential backoff (5s doubling to a 30s cap); a failed renewal falls back
// to a fresh registration slot.
func (r *Registration) Run(ctx context.Context) {
	retryDelay := registrationRetryDelay
	for {
		interval, err := r.publish(ctx)
		if err == nil {
			r.setRegistered(true)
			retryDelay = registrationRetryDelay
			interval = r.renewLoop(ctx, interval)
			if ctx.Err() != nil {
				return
			}
			err = fmt.Errorf("lease renewal failed after %s", interval)
		} else if ctx.Err() != nil {
			return
		}

		r.setRegistered(false)
		logging.Log.Warn().
			Err(err).
			Str("instance", r.cfg.Relay.ID).
			Str("endpoint", r.endpoint).
			Dur("retryIn", retryDelay).
			Msg("Relay registration failed")

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

// publish sends the current health report and renews the lease in one call: the
// control endpoint is an idempotent upsert, so a refreshed endpoint, region, or
// weight travels with every heartbeat.
func (r *Registration) publish(ctx context.Context) (time.Duration, error) {
	failures := r.reportHealth(ctx)
	r.logHealthChange(failures)
	report := relayRegistration{
		Endpoint: r.cfg.Relay.PublicHost,
		Port:     r.cfg.Relay.PublicPort,
		Region:   r.cfg.Relay.Region,
		Weight:   r.cfg.Relay.Weight,
		Healthy:  len(failures) == 0,
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return 0, err
	}

	callCtx, cancel := context.WithTimeout(ctx, registrationCallTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(callCtx, http.MethodPut, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.cfg.Discovery.RegistrationToken))

	response, err := r.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxRegistrationBody))
		return 0, fmt.Errorf("registration rejected: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	var decoded relayRegistrationResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxRegistrationBody)).Decode(&decoded); err != nil {
		return 0, fmt.Errorf("registration response: %w", err)
	}

	logging.Log.Info().
		Str("instance", r.cfg.Relay.ID).
		Str("tcp", r.cfg.Relay.PublicAddress()).
		Str("region", r.cfg.Relay.Region).
		Bool("healthy", report.Healthy).
		Time("expiresAt", time.UnixMilli(decoded.LeaseExpiresAtUnixMs)).
		Msg("Relay registered with the catalog")
	return RenewalInterval(decoded.LeaseExpiresAtUnixMs, time.Now()), nil
}

// renewLoop heartbeats until ctx ends or a renewal fails, then returns the
// interval it was using so the caller can report it.
func (r *Registration) renewLoop(ctx context.Context, interval time.Duration) time.Duration {
	for {
		select {
		case <-ctx.Done():
			return interval
		case <-time.After(interval):
		}

		next, err := r.publish(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return interval
			}
			logging.Log.Warn().
				Err(err).
				Str("instance", r.cfg.Relay.ID).
				Msg("Relay lease renewal failed; re-registering")
			return interval
		}
		interval = next
	}
}

// reportHealth runs the upstream check this node's health is built from.
func (r *Registration) reportHealth(ctx context.Context) map[string]string {
	if r.health == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, registrationCallTimeout)
	defer cancel()
	return r.health(callCtx)
}

// logHealthChange logs a health transition with the per-target reasons. The
// catalog stores only the boolean, so without this the failure detail would
// never leave the node; it is logged on transition, not on every heartbeat.
func (r *Registration) logHealthChange(failures map[string]string) {
	healthy := len(failures) == 0

	r.mu.Lock()
	changed := r.reported == nil || *r.reported != healthy
	r.reported = &healthy
	r.mu.Unlock()

	if !changed {
		return
	}
	if healthy {
		logging.Log.Info().Str("instance", r.cfg.Relay.ID).Msg("Relay upstreams reachable")
		return
	}
	logging.Log.Warn().
		Str("instance", r.cfg.Relay.ID).
		Interface("failures", failures).
		Msg("Relay upstreams unreachable")
}

// Deregister withdraws this relay so it leaves the catalog before its lease
// expires. It is best effort: the lease expires on its own.
func (r *Registration) Deregister(ctx context.Context) {
	if !r.isRegistered() {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, deregisterTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(callCtx, http.MethodDelete, r.endpoint, nil)
	if err != nil {
		logging.Log.Warn().Err(err).Str("instance", r.cfg.Relay.ID).Msg("Relay deregistration failed")
		return
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.cfg.Discovery.RegistrationToken))

	response, err := r.client.Do(request)
	if err != nil {
		logging.Log.Warn().Err(err).Str("instance", r.cfg.Relay.ID).Msg("Relay deregistration failed")
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxRegistrationBody))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		logging.Log.Warn().
			Int("status", response.StatusCode).
			Str("instance", r.cfg.Relay.ID).
			Msg("Relay deregistration rejected")
		return
	}
	r.setRegistered(false)
}

// RenewalInterval is a third of the remaining lease, never below one second.
// Without a usable expiry it falls back to a conservative cadence. Pure, so the
// heartbeat rhythm can be unit-tested.
func RenewalInterval(leaseExpiresAtUnixMs int64, now time.Time) time.Duration {
	if leaseExpiresAtUnixMs <= 0 {
		return defaultRenewalInterval
	}
	remaining := time.UnixMilli(leaseExpiresAtUnixMs).Sub(now)
	if remaining <= 0 {
		return time.Second
	}
	return clampRenewal(remaining.Seconds() / 3)
}

func clampRenewal(seconds float64) time.Duration {
	if seconds < 1 {
		return time.Second
	}
	return time.Duration(seconds * float64(time.Second))
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
