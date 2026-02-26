package wsgateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractTokenPriority_QueryOverHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws?tk=query-token", nil)
	req.Header.Set("Authorization", "Bearer header-token")

	got, ok := extractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Token != "query-token" {
		t.Fatalf("expected query token, got %q", got.Token)
	}
	if got.Type != TokenTypeAuthKey {
		t.Fatalf("expected auth key, got %q", got.Type)
	}
}

func TestExtractToken_BearerOidcType(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")

	got, ok := extractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Type != TokenTypeOidcKey {
		t.Fatalf("expected oidc key, got %q", got.Type)
	}
}

func TestExtractToken_CookieFallback(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.AddCookie(&http.Cookie{Name: "AuthToken", Value: "cookie-token"})

	got, ok := extractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Token != "cookie-token" {
		t.Fatalf("expected cookie token, got %q", got.Token)
	}
}
