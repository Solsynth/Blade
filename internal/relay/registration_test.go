package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeGateway stands in for the public relay control endpoint.
type fakeGateway struct {
	mu             sync.Mutex
	registrations  []relayRegistration
	authorizations []string
	deregistered   []string
	status         int
	leaseMillis    int64
}

func (f *fakeGateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authorizations = append(f.authorizations, r.Header.Get("Authorization"))
		f.mu.Unlock()

		switch r.Method {
		case http.MethodPut:
			var body relayRegistration
			if err := json.NewDecoder(io.LimitReader(r.Body, maxRegistrationBody)).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.registrations = append(f.registrations, body)
			status := f.status
			lease := f.leaseMillis
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(relayRegistrationResponse{
				ID:                   "jp-01",
				LeaseExpiresAtUnixMs: lease,
			})
		case http.MethodDelete:
			f.mu.Lock()
			f.deregistered = append(f.deregistered, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (f *fakeGateway) snapshot() ([]relayRegistration, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayRegistration(nil), f.registrations...),
		append([]string(nil), f.authorizations...),
		append([]string(nil), f.deregistered...)
}

func registrationConfig(t *testing.T, gatewayURL string) Config {
	t.Helper()
	cfg := validConfig()
	cfg.Relay.Region = "jp"
	cfg.Discovery.Enabled = true
	cfg.Discovery.URL = gatewayURL
	return *cfg
}

func TestRegistrationPublishesRelayInstance(t *testing.T) {
	gateway := &fakeGateway{leaseMillis: time.Now().Add(30 * time.Second).UnixMilli()}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()

	cfg := registrationConfig(t, server.URL)
	registration, err := NewRegistration(cfg, func(context.Context) bool { return true })
	if err != nil {
		t.Fatalf("NewRegistration() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go registration.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		registrations, authorizations, _ := gateway.snapshot()
		if len(registrations) > 0 {
			first := registrations[0]
			if first.Endpoint != "relay-jp.solian.app" || first.Port != 443 {
				t.Fatalf("registered address = %s:%d", first.Endpoint, first.Port)
			}
			if first.Region != "jp" || first.Weight != 1 || !first.Healthy {
				t.Fatalf("registered payload = %+v", first)
			}
			if len(authorizations) == 0 || authorizations[0] != "Bearer secret" {
				t.Fatalf("authorization = %v", authorizations)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The control path carries the instance id Blade keys on.
	if got := registration.endpoint; got != server.URL+"/relays/jp-01" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestRegistrationReportsUnhealthyUpstreams(t *testing.T) {
	gateway := &fakeGateway{leaseMillis: time.Now().Add(30 * time.Second).UnixMilli()}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()

	cfg := registrationConfig(t, server.URL)
	registration, err := NewRegistration(cfg, func(context.Context) bool { return false })
	if err != nil {
		t.Fatalf("NewRegistration() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go registration.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		registrations, _, _ := gateway.snapshot()
		if len(registrations) > 0 {
			if registrations[0].Healthy {
				t.Fatal("a failing upstream check must be reported as unhealthy")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRegistrationRenewsAndDeregisters(t *testing.T) {
	gateway := &fakeGateway{leaseMillis: time.Now().Add(1500 * time.Millisecond).UnixMilli()}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()

	cfg := registrationConfig(t, server.URL)
	registration, err := NewRegistration(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistration() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go registration.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for {
		registrations, _, _ := gateway.snapshot()
		if len(registrations) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected a renewal heartbeat, saw %d registration(s)", len(registrations))
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	time.Sleep(50 * time.Millisecond)

	deregisterCtx, cancelDeregister := context.WithTimeout(context.Background(), time.Second)
	defer cancelDeregister()
	registration.Deregister(deregisterCtx)

	_, _, deregistered := gateway.snapshot()
	if len(deregistered) != 1 || deregistered[0] != "/relays/jp-01" {
		t.Fatalf("deregistered = %v", deregistered)
	}
}

func TestRegistrationBacksOffAndRecovers(t *testing.T) {
	gateway := &fakeGateway{status: http.StatusUnauthorized}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()

	cfg := registrationConfig(t, server.URL)
	registration, err := NewRegistration(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistration() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	registration.Run(ctx)

	registrations, _, _ := gateway.snapshot()
	if len(registrations) == 0 {
		t.Fatal("a rejected registration must be retried")
	}
	if len(registrations) > 2 {
		t.Fatalf("attempts = %d, want backoff to keep the retry rate low", len(registrations))
	}
}

func TestRenewalInterval(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		expires int64
		want    time.Duration
	}{
		{"third of the remaining lease", now.Add(30 * time.Second).UnixMilli(), 10 * time.Second},
		{"never below one second", now.Add(2 * time.Second).UnixMilli(), time.Second},
		{"expired lease", now.Add(-time.Second).UnixMilli(), time.Second},
		{"missing expiry falls back", 0, defaultRenewalInterval},
	}
	for _, tc := range cases {
		if got := RenewalInterval(tc.expires, now); got != tc.want {
			t.Errorf("%s: RenewalInterval() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNewRegistrationRejectsRelativeURL(t *testing.T) {
	cfg := validConfig()
	cfg.Discovery.URL = "api.solian.app"
	if _, err := NewRegistration(*cfg, nil); err == nil {
		t.Fatal("NewRegistration() must reject a relative discovery.url")
	}
}
