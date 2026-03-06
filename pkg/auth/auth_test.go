package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractTokenPriorityQueryOverHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws?tk=query-token", nil)
	req.Header.Set("Authorization", "Bearer header-token")

	got, ok := ExtractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Token != "query-token" {
		t.Fatalf("expected query token, got %q", got.Token)
	}
	if got.Type != TokenTypeLegacyUserToken {
		t.Fatalf("expected legacy user token, got %q", got.Type)
	}
}

func TestExtractTokenBearerJWTType(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")

	got, ok := ExtractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Type != TokenTypeUserJWT {
		t.Fatalf("expected user jwt, got %q", got.Type)
	}
}

func TestExtractTokenBotScheme(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bot key.jwt.token")

	got, ok := ExtractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Type != TokenTypeAPIKeyJWT {
		t.Fatalf("expected api key jwt, got %q", got.Type)
	}
}

func TestExtractTokenLegacyAkField(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "AkField legacy-api-key")

	got, ok := ExtractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Token != "legacy-api-key" {
		t.Fatalf("expected legacy-api-key, got %q", got.Token)
	}
	if got.Type != TokenTypeLegacyAPIKey {
		t.Fatalf("expected legacy api key, got %q", got.Type)
	}
}

func TestExtractTokenCookieFallback(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.AddCookie(&http.Cookie{Name: "AuthToken", Value: "cookie-token"})

	got, ok := ExtractToken(req)
	if !ok {
		t.Fatal("expected token")
	}
	if got.Token != "cookie-token" {
		t.Fatalf("expected cookie token, got %q", got.Token)
	}
	if got.Type != TokenTypeLegacyUserToken {
		t.Fatalf("expected legacy user token, got %q", got.Type)
	}
}

func TestNormalizeAuthGRPCTargetWithHTTPS(t *testing.T) {
	target, useTLS := NormalizeAuthGRPCTarget("https://localhost:7003", false)
	if target != "localhost:7003" {
		t.Fatalf("expected localhost:7003, got %q", target)
	}
	if !useTLS {
		t.Fatal("expected TLS to be enabled for https target")
	}
}

func TestNormalizeAuthGRPCTargetWithPlainHost(t *testing.T) {
	target, useTLS := NormalizeAuthGRPCTarget("localhost:7003", true)
	if target != "localhost:7003" {
		t.Fatalf("expected localhost:7003, got %q", target)
	}
	if !useTLS {
		t.Fatal("expected TLS flag to preserve explicit config for plain host target")
	}
}
