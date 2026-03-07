package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.solsynth.dev/sosys/blade/internal/config"
	"github.com/gin-gonic/gin"
)

type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
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
