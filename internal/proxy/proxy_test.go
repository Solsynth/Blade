package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.solsynth.dev/solarnetwork/blade/internal/config"
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
		specialRoutes: config.SpecialRoutesConfig{
			Routes: []config.RouteRule{
				{Path: "/ws", Service: "ring", Target: "/api/ws", Prefix: false},
			},
		},
	}

	r := gin.New()
	r.NoRoute(p.Handler())

	req := httptest.NewRequest(http.MethodGet, "/ws?tk=abc", nil)
	rec := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHttp(rec, req)

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
