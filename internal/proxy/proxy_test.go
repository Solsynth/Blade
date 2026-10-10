package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/config"
	discovery "srv.solsynth.dev/sosys/blade/internal/discovery"
)

type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
}

func TestProxy_RegisteredUnhealthyInstanceDoesNotFallBackToStaticTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	registry := discovery.NewRegistry(nil, "test:discovery", 0)
	if _, _, err := registry.Register(t.Context(), &gen.DyServiceInstance{
		Service: "sphere", InstanceId: "sphere-1", HttpEndpoint: "http://unhealthy.example",
	}, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	p := &Proxy{
		serviceURLs: map[string]string{"sphere": "http://static.example"},
		registry:    registry,
	}
	r := gin.New()
	r.NoRoute(p.Handler())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sphere/feed", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for an unhealthy registered service, got %d", rec.Code)
	}
}

func TestProxy_DiscoveredServiceWithoutLocalConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := discovery.NewRegistry(nil, "test:discovery", 0)
	if _, _, err := registry.Register(t.Context(), &gen.DyServiceInstance{
		Service: "personality", InstanceId: "personality-1", HttpEndpoint: upstream.URL,
	}, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.SetHealth(t.Context(), "personality", "personality-1", true); err != nil {
		t.Fatalf("SetHealth() error = %v", err)
	}

	p := &Proxy{serviceURLs: map[string]string{}, registry: registry}
	r := gin.New()
	r.NoRoute(p.Handler())
	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/personality/conversations", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected discovered service response 200, got %d", rec.Code)
	}
	if gotPath != "/api/conversations" {
		t.Fatalf("expected discovered upstream path /api/conversations, got %q", gotPath)
	}
}

func (r *closeNotifyRecorder) CloseNotify() <-chan bool {
	ch := make(chan bool, 1)
	return ch
}

func TestProxyRequest_TargetWithPortAndPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	var gotQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/sphere/timeline?take=20&showFediverse=true", nil)
	ctx.Request = req

	p := &Proxy{}
	p.proxyRequest(ctx, upstream.URL+"/api/timeline")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if gotPath != "/api/timeline" {
		t.Fatalf("expected upstream path /api/timeline, got %q", gotPath)
	}
	if gotQuery != "take=20&showFediverse=true" {
		t.Fatalf("expected forwarded query, got %q", gotQuery)
	}
}

func TestProxyRequest_StripsClientTrustHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/sphere/feed", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("X-Account-Id", "00000000-0000-0000-0000-000000000001")
	req.Header.Set("X-User-Id", "00000000-0000-0000-0000-000000000002")
	req.Header.Set("X-Account-Email", "ceo@example.com")
	req.Header.Set("X-Auth-Role", "root")
	req.Header.Set("X-Real-IP", "10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	ctx.Request = req

	p := &Proxy{}
	p.proxyRequest(ctx, upstream.URL+"/api/feed")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	for _, name := range []string{"X-Account-Id", "X-User-Id", "X-Account-Email", "X-Auth-Role"} {
		if value := got.Get(name); value != "" {
			t.Fatalf("upstream received client-supplied %s = %q", name, value)
		}
	}
	if value := got.Get("X-Real-IP"); value != "203.0.113.7" {
		t.Fatalf("X-Real-IP = %q, want the connection address", value)
	}
	if value := got.Get("X-Forwarded-For"); value != "203.0.113.7" {
		t.Fatalf("X-Forwarded-For = %q, want the connection address", value)
	}
	if value := got.Get("X-Forwarded-Proto"); value != "http" {
		t.Fatalf("X-Forwarded-Proto = %q, want the connection scheme", value)
	}
	if value := got.Get("X-Forwarded-Host"); value != "example.com" {
		t.Fatalf("X-Forwarded-Host = %q, want the request host", value)
	}
}

func TestProxyRequest_TrustedProxyHopsResolveClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/sphere/feed", nil)
	// The peer is the trusted proxy; the real client is what it saw.
	req.RemoteAddr = "192.0.2.10:5000"
	req.Header.Set("X-Forwarded-For", "10.9.9.9, 198.51.100.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "api.solian.app")
	ctx.Request = req

	p := &Proxy{trustedProxyHops: 1}
	p.proxyRequest(ctx, upstream.URL+"/api/feed")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	// One trusted hop: the rightmost entry is what the trusted proxy appended,
	// and the entry to its left is client-supplied noise.
	if value := got.Get("X-Forwarded-For"); value != "198.51.100.9" {
		t.Fatalf("X-Forwarded-For = %q, want the address the trusted proxy observed", value)
	}
	if value := got.Get("X-Real-IP"); value != "198.51.100.9" {
		t.Fatalf("X-Real-IP = %q, want the resolved client address", value)
	}
	if value := got.Get("X-Forwarded-Proto"); value != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want the edge's protocol", value)
	}
	if value := got.Get("X-Forwarded-Host"); value != "api.solian.app" {
		t.Fatalf("X-Forwarded-Host = %q, want the edge's host", value)
	}
}

func TestProxyRequest_FallsBackToPeerWhenForwardedForIsTooShort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/sphere/feed", nil)
	req.RemoteAddr = "192.0.2.10:5000"
	req.Header.Set("X-Forwarded-For", "10.9.9.9")
	ctx.Request = req

	p := &Proxy{trustedProxyHops: 2}
	p.proxyRequest(ctx, upstream.URL+"/api/feed")

	if value := got.Get("X-Forwarded-For"); value != "192.0.2.10" {
		t.Fatalf("X-Forwarded-For = %q, want the peer when the chain is too short", value)
	}
}

func TestProxyRequest_ReusesUpstreamConnections(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var accepted int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&accepted, 1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	p := &Proxy{transport: newProxyTransport()}
	for i := range 20 {
		rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/sphere/notifications", nil)
		p.proxyRequest(ctx, upstream.URL+"/api/notifications")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected status 200, got %d", i, rec.Code)
		}
	}

	if got := atomic.LoadInt32(&accepted); got != 1 {
		t.Fatalf("expected one reused upstream connection, got %d", got)
	}
}

func TestSpecialRouteWS_ProxiesToConfiguredTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	var gotQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := &Proxy{
		serviceURLs: map[string]string{
			"ring": upstream.URL,
		},
		routes: []config.RouteRule{
			{Path: "/ws", Service: "ring", Target: "/api/ws", Prefix: false},
		},
	}

	r := gin.New()
	r.NoRoute(p.Handler())

	req := httptest.NewRequest(http.MethodGet, "/ws?tk=abc", nil)
	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 from upstream, got %d", rec.Code)
	}
	if gotPath != "/api/ws" {
		t.Fatalf("expected upstream path /api/ws, got %q", gotPath)
	}
	if gotQuery != "tk=abc" {
		t.Fatalf("expected query tk=abc, got %q", gotQuery)
	}
}

func TestMaintenanceFullMode_BlocksAllRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	p := &Proxy{
		serviceURLs: map[string]string{
			"sphere": "http://example.invalid",
		},
		maintenance: config.MaintenanceConfig{
			Enabled: true,
			Mode:    "full",
		},
	}

	r := gin.New()
	r.NoRoute(p.Handler())

	req := httptest.NewRequest(http.MethodGet, "/sphere/feed", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}
}

func TestMaintenanceServiceMode_BlocksConfiguredServiceOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sphereHits := 0
	sphereUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sphereHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer sphereUpstream.Close()

	ringHits := 0
	ringUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ringHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer ringUpstream.Close()

	p := &Proxy{
		serviceURLs: map[string]string{
			"sphere": sphereUpstream.URL,
			"ring":   ringUpstream.URL,
		},
		maintenance: config.MaintenanceConfig{
			Enabled:  true,
			Mode:     "service",
			Services: []string{"sphere"},
		},
		blockedSet: toServiceSet([]string{"sphere"}),
	}

	r := gin.New()
	r.NoRoute(p.Handler())

	blockedReq := httptest.NewRequest(http.MethodGet, "/sphere/feed", nil)
	blockedRec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(blockedRec, blockedReq)

	if blockedRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected blocked service status 503, got %d", blockedRec.Code)
	}
	if sphereHits != 0 {
		t.Fatalf("expected blocked service not to reach upstream, got hits=%d", sphereHits)
	}

	allowedReq := httptest.NewRequest(http.MethodGet, "/ring/feed", nil)
	allowedRec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(allowedRec, allowedReq)

	if allowedRec.Code != http.StatusOK {
		t.Fatalf("expected allowed service status 200, got %d", allowedRec.Code)
	}
	if ringHits != 1 {
		t.Fatalf("expected allowed service to reach upstream once, got hits=%d", ringHits)
	}
}
