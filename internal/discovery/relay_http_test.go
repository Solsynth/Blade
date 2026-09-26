package discovery

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gen "src.solsynth.dev/sosys/go/proto"
)

const relayTestToken = "relay-secret"

// relayInstance builds the minimal instance record the relay API publishes.
func relayInstance(service, id, tcpEndpoint string) *gen.DyServiceInstance {
	return &gen.DyServiceInstance{
		Service:    service,
		InstanceId: id,
		Endpoints:  map[string]string{"tcp": tcpEndpoint},
	}
}

func newRelayTestRouter(t *testing.T) (*gin.Engine, *Registry, *Catalog) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	registry := NewRegistry(nil, "test:discovery", time.Minute)
	catalog := NewCatalog(registry, "relay")
	api := NewRelayAPI(registry, "relay", relayTestToken, time.Minute)

	router := gin.New()
	api.RegisterRoutes(router)
	return router, registry, catalog
}

func relayRequest(t *testing.T, router *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}

	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestRelayAPIRegistersRelay(t *testing.T) {
	router, registry, catalog := newRelayTestRouter(t)

	recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, map[string]any{
		"endpoint": "relay-jp.solian.app",
		"port":     7443,
		"region":   "JP",
		"weight":   3,
		"healthy":  true,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		ID                   string `json:"id"`
		LeaseExpiresAtUnixMs int64  `json:"lease_expires_at_unix_ms"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ID != "jp-01" {
		t.Fatalf("id = %q", response.ID)
	}
	if response.LeaseExpiresAtUnixMs <= time.Now().UnixMilli() {
		t.Fatalf("lease expiry = %d, want a future timestamp", response.LeaseExpiresAtUnixMs)
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	instance := instances[0]
	if got := Endpoint(instance, "tcp"); got != "relay-jp.solian.app:7443" {
		t.Fatalf("tcp endpoint = %q", got)
	}
	if instance.GetWeight() != 3 {
		t.Fatalf("weight = %d", instance.GetWeight())
	}
	if instance.GetMetadata()["region"] != "jp" {
		t.Fatalf("region = %q, want the normalized region", instance.GetMetadata()["region"])
	}
	// Health is the relay's own report: nothing here probes it.
	if !instance.GetHealthy() {
		t.Fatal("self-reported health must be stored as reported")
	}

	relays, err := catalog.List(t.Context())
	if err != nil {
		t.Fatalf("catalog List() error = %v", err)
	}
	if len(relays) != 1 || relays[0].ID != "jp-01" || relays[0].Port != 7443 || !relays[0].Healthy {
		t.Fatalf("catalog = %+v", relays)
	}
	if relays[0].Endpoint != "relay-jp.solian.app" || relays[0].Region != "jp" {
		t.Fatalf("catalog entry = %+v", relays[0])
	}
}

func TestRelayAPIReportsUnhealthyRelay(t *testing.T) {
	router, registry, _ := newRelayTestRouter(t)

	recorder := relayRequest(t, router, http.MethodPut, "/relays/eu-01", relayTestToken, map[string]any{
		"endpoint": "relay-eu.solian.app",
		"port":     443,
		"healthy":  false,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	if instances[0].GetHealthy() {
		t.Fatal("a relay reporting unhealthy must not be stored healthy")
	}
	if instances[0].GetWeight() != 1 {
		t.Fatalf("weight = %d, want the default", instances[0].GetWeight())
	}
}

func TestRelayAPIRejectsBadCredentials(t *testing.T) {
	router, registry, _ := newRelayTestRouter(t)
	body := map[string]any{"endpoint": "relay-jp.solian.app", "port": 443}

	for name, token := range map[string]string{"missing": "", "wrong": "nope"} {
		recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", token, body)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s credential: status = %d, want 401", name, recorder.Code)
		}
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances = %d, want none", len(instances))
	}
}

func TestRelayAPIRefusesWithoutConfiguredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := NewRegistry(nil, "test:discovery", time.Minute)
	router := gin.New()
	NewRelayAPI(registry, "relay", "  ", time.Minute).RegisterRoutes(router)

	recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, map[string]any{
		"endpoint": "relay-jp.solian.app",
		"port":     443,
	})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestRelayAPIValidatesPayload(t *testing.T) {
	router, _, _ := newRelayTestRouter(t)

	cases := map[string]map[string]any{
		"missing endpoint": {"port": 443},
		"port too high":    {"endpoint": "relay-jp.solian.app", "port": 70000},
		"port zero":        {"endpoint": "relay-jp.solian.app", "port": 0},
	}
	for name, body := range cases {
		recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, recorder.Code)
		}
	}

	recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, "not an object")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", recorder.Code)
	}
}

func TestRelayAPIDeregisters(t *testing.T) {
	router, registry, catalog := newRelayTestRouter(t)

	if recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, map[string]any{
		"endpoint": "relay-jp.solian.app",
		"port":     7443,
		"healthy":  true,
	}); recorder.Code != http.StatusOK {
		t.Fatalf("register status = %d", recorder.Code)
	}

	recorder := relayRequest(t, router, http.MethodDelete, "/relays/jp-01", relayTestToken, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances = %d, want none after deregistration", len(instances))
	}
	relays, err := catalog.List(t.Context())
	if err != nil {
		t.Fatalf("catalog List() error = %v", err)
	}
	if len(relays) != 0 {
		t.Fatalf("catalog = %+v, want empty", relays)
	}

	// A withdrawal with a bad credential must not remove anything.
	if recorder := relayRequest(t, router, http.MethodPut, "/relays/jp-01", relayTestToken, map[string]any{
		"endpoint": "relay-jp.solian.app",
		"port":     7443,
	}); recorder.Code != http.StatusOK {
		t.Fatalf("re-register status = %d", recorder.Code)
	}
	if recorder := relayRequest(t, router, http.MethodDelete, "/relays/jp-01", "wrong", nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized delete status = %d", recorder.Code)
	}
	instances, _ = registry.List(t.Context(), "relay")
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want the relay to survive a rejected withdrawal", len(instances))
	}
}

func TestRegisterSelfReportedKeepsReportedHealth(t *testing.T) {
	registry := NewRegistry(nil, "test:discovery", time.Minute)

	instance := relayInstance("relay", "jp-01", "relay-jp.solian.app:7443")
	instance.Healthy = true
	registered, _, err := registry.RegisterSelfReported(t.Context(), instance, time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterSelfReported() error = %v", err)
	}
	if !registered.GetHealthy() {
		t.Fatal("RegisterSelfReported must store the reported health")
	}

	// The probing path still forces new instances to start unhealthy.
	probed := relayInstance("relay", "eu-01", "relay-eu.solian.app:443")
	probed.Healthy = true
	registered, _, err = registry.Register(t.Context(), probed, time.Minute)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registered.GetHealthy() {
		t.Fatal("Register must leave probing ownership of health")
	}
}
