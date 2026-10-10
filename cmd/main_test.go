package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/config"
	discovery "srv.solsynth.dev/sosys/blade/internal/discovery"
	"srv.solsynth.dev/sosys/blade/internal/health"
)

const testInternalToken = "internal-secret"

// GET /health is the public client-facing status surface: an anonymous caller
// gets the full document, including the per-service checks map.
func TestHealthEndpointServesChecksToAnonymousCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := health.NewReadinessStore([]string{"ring"})
	store.UpdateService(health.ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Now()})
	store.UpdateService(health.ServiceState{ServiceName: "sphere", IsHealthy: true, LastChecked: time.Now()})

	router := gin.New()
	registerHealthRoutes(router, store, &config.Config{})

	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/health", nil))
	if anonymous.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d, body = %s", anonymous.Code, anonymous.Body.String())
	}
	if !strings.Contains(anonymous.Body.String(), `"status":"pass"`) {
		t.Fatalf("anonymous body = %s, want the overall status", anonymous.Body.String())
	}
	if !strings.Contains(anonymous.Body.String(), `"checks"`) || !strings.Contains(anonymous.Body.String(), `"ring"`) {
		t.Fatalf("anonymous body = %s, want the per-service checks map", anonymous.Body.String())
	}
}

// The access log is the audit record: a client must not be able to forge the
// address in it via X-Forwarded-For.
func TestAccessLogUsesAuthoritativeClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name      string
		hops      int
		forwarded string
		want      string
		unwanted  string
	}{
		{
			name:      "no trusted hop ignores a spoofed chain",
			hops:      0,
			forwarded: "9.9.9.9",
			want:      "203.0.113.7",
			unwanted:  "9.9.9.9",
		},
		{
			name:      "one trusted hop uses what the proxy appended",
			hops:      1,
			forwarded: "9.9.9.9, 198.51.100.7",
			want:      "198.51.100.7",
			unwanted:  "9.9.9.9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer

			router := gin.New()
			if err := router.SetTrustedProxies(nil); err != nil {
				t.Fatalf("SetTrustedProxies() error = %v", err)
			}
			router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
				Formatter: accessLogFormatter(tc.hops),
				Output:    &logged,
			}))
			router.GET("/ping", func(c *gin.Context) {
				c.String(http.StatusOK, "pong")
			})

			request := httptest.NewRequest(http.MethodGet, "/ping", nil)
			request.RemoteAddr = "203.0.113.7:4321"
			request.Header.Set("X-Forwarded-For", tc.forwarded)
			router.ServeHTTP(httptest.NewRecorder(), request)

			line := logged.String()
			if !strings.Contains(line, tc.want) {
				t.Fatalf("log line = %q, want client %q", line, tc.want)
			}
			if strings.Contains(line, tc.unwanted) {
				t.Fatalf("log line = %q, must not contain the spoofed %q", line, tc.unwanted)
			}
			for _, shape := range []string{"| 200 |", "GET", "/ping"} {
				if !strings.Contains(line, shape) {
					t.Fatalf("log line = %q, want the default shape to carry %q", line, shape)
				}
			}
		})
	}
}

// newRelayTestCatalog stubs a catalog with one registered relay.
func newRelayTestCatalog(t *testing.T) *discovery.Catalog {
	t.Helper()
	registry := discovery.NewRegistry(nil, "test:discovery", time.Minute)
	if _, _, err := registry.RegisterSelfReported(t.Context(), &gen.DyServiceInstance{
		Service:    "relay",
		InstanceId: "jp-01",
		Weight:     3,
		Endpoints:  map[string]string{"tcp": "relay-jp.solian.app:443"},
		Metadata:   map[string]string{"region": "jp"},
	}, time.Minute, true); err != nil {
		t.Fatalf("RegisterSelfReported() error = %v", err)
	}
	return discovery.NewCatalog(registry, "relay")
}

// GET /relays is the public client-facing discovery contract: it must answer
// anonymously with the fields the client dials (endpoint, port) and sorts by
// (region, weight, healthy). Do not gate it.
func TestRelaysEndpointServesCatalogAnonymously(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	registerRelaysRoute(router, newRelayTestCatalog(t))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/relays", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, field := range []string{"relay-jp.solian.app", `"port":443`, `"region":"jp"`, `"healthy":true`} {
		if !strings.Contains(recorder.Body.String(), field) {
			t.Fatalf("anonymous body = %s, want the client discovery field %s", recorder.Body.String(), field)
		}
	}
}

// The control plane is the opposite: relays authenticate with the discovery
// credential, and anonymous writes must be rejected.
func TestRelayControlPlaneRejectsAnonymousCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	registry := discovery.NewRegistry(nil, "test:discovery", time.Minute)
	router := gin.New()
	discovery.NewRelayAPI(registry, "relay", testInternalToken, time.Minute).RegisterRoutes(router)

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, "/relays/jp-01", nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s /relays/jp-01 anonymous status = %d, want 401", method, recorder.Code)
		}
	}

	instances, err := registry.List(t.Context(), "relay")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances = %d, want none after rejected anonymous writes", len(instances))
	}
}
