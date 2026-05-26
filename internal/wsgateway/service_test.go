package wsgateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type stubTokenAuthenticator struct {
	result    *dyauth.AuthResult
	err       error
	lastToken string
}

func (s *stubTokenAuthenticator) Authenticate(_ context.Context, tokenInfo dyauth.TokenInfo, _ *http.Request) (*dyauth.AuthResult, error) {
	s.lastToken = tokenInfo.Token
	return s.result, s.err
}

func TestServiceNormalizeDeviceID_UsesProvidedValue(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)

	got := svc.normalizeDeviceID("  device-123  ")
	if got != "device-123" {
		t.Fatalf("expected trimmed device id, got %q", got)
	}
}

func TestServiceNormalizeDeviceID_GeneratesUUIDWhenMissing(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)

	got := svc.normalizeDeviceID("   ")
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("expected generated UUID, got %q: %v", got, err)
	}
}

func TestServiceNormalizeDeviceID_GeneratesUUIDWithDeviceAltSuffix(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)

	got := svc.normalizeDeviceID("+watch")
	const suffix = "+watch"
	if len(got) <= len(suffix) || got[len(got)-len(suffix):] != suffix {
		t.Fatalf("expected generated id to keep %q suffix, got %q", suffix, got)
	}

	base := got[:len(got)-len(suffix)]
	if _, err := uuid.Parse(base); err != nil {
		t.Fatalf("expected uuid base in generated id %q: %v", got, err)
	}
}

func TestServiceTryAddUniqueDevice_RejectsDuplicateDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  account1,
		deviceID: "d1",
		probe: func() bool {
			return true
		},
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d1", nil); ok {
		t.Fatal("expected duplicate device id to be rejected")
	}
}

func TestServiceTryAddUniqueDevice_AcceptsDifferentDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	if _, ok := svc.TryAddUniqueDevice(account1, "d1", nil); !ok {
		t.Fatal("expected first connection to be accepted")
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d2", nil); !ok {
		t.Fatal("expected different device id to be accepted")
	}
}

func TestServiceTryAddUniqueDevice_ReplacesStaleDuplicateDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  account1,
		deviceID: "d1",
		probe: func() bool {
			return false
		},
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d1", nil); !ok {
		t.Fatal("expected stale duplicate device id to be replaced")
	}

	accounts := svc.GetAccountsByDevice("d1")
	if len(accounts) != 1 || accounts[0] != "u2" {
		t.Fatalf("expected device d1 to belong only to u2 after replacement, got %#v", accounts)
	}
}

func TestServiceGetAccountConnections_ExcludesDeviceIDs(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "d1",
	}
	svc.connections[connectionKey{accountID: "u1", deviceID: "d2"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "d2",
	}
	svc.connections[connectionKey{accountID: "u2", deviceID: "d3"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u2"},
		deviceID: "d3",
	}

	got := svc.getAccountConnections("u1", []string{" d2 ", "", "missing"})
	if len(got) != 1 {
		t.Fatalf("expected 1 included connection, got %d", len(got))
	}
	if got[0].deviceID != "d1" {
		t.Fatalf("expected d1 to receive packet, got %q", got[0].deviceID)
	}
}

func TestServiceDisconnectSession_MatchesSessionID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	done := make(chan struct{})
	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:   &gen.DyAccount{Id: "u1"},
		sessionID: "s1",
		deviceID:  "d1",
		done:      done,
	}

	disconnected := svc.DisconnectSession("s1", "", "", "logged out")
	if disconnected != 1 {
		t.Fatalf("expected 1 disconnected connection, got %d", disconnected)
	}
	if svc.GetAccountIsConnected("u1") {
		t.Fatal("expected connection to be removed after disconnect")
	}

	select {
	case <-done:
	default:
		t.Fatal("expected connection close signal after disconnect")
	}
}

func TestServiceDisconnectSession_FallsBackToAccountAndDevicePrefix(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	done := make(chan struct{})
	svc.connections[connectionKey{accountID: "u1", deviceID: "device-1+watch"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "device-1+watch",
		done:     done,
	}

	disconnected := svc.DisconnectSession("", "u1", "device-1", "logged out")
	if disconnected != 1 {
		t.Fatalf("expected fallback disconnect to match watch suffix, got %d", disconnected)
	}

	select {
	case <-done:
	default:
		t.Fatal("expected connection close signal after fallback disconnect")
	}
}

func TestServiceReauthenticateConnection_RefreshesExpiry(t *testing.T) {
	newExpiry := time.Now().Add(10 * time.Minute).UTC()
	authenticator := &stubTokenAuthenticator{
		result: &dyauth.AuthResult{
			Account: &gen.DyAccount{Id: "u1"},
			Session: &gen.DyAuthSession{
				Id:        "s2",
				AccountId: "u1",
				ExpiredAt: timestamppb.New(newExpiry),
			},
		},
	}
	svc := NewService(Config{}, nil, nil, nil, authenticator, nil, nil)
	entry := &wsConnection{
		account:   &gen.DyAccount{Id: "u1"},
		sessionID: "s1",
		deviceID:  "d1",
		expiresAt: time.Now().Add(-time.Minute).UTC(),
		tokenInfo: dyauth.TokenInfo{Token: "token-1", Type: dyauth.TokenTypeAuthKey},
		authReq:   &http.Request{Header: make(http.Header)},
		done:      make(chan struct{}),
	}

	if err := svc.reauthenticateConnection(context.Background(), entry); err != nil {
		t.Fatalf("expected reauthentication to succeed, got %v", err)
	}
	if authenticator.lastToken != "token-1" {
		t.Fatalf("expected original token to be reused, got %q", authenticator.lastToken)
	}
	if got := entry.getSessionID(); got != "s2" {
		t.Fatalf("expected session id to refresh to s2, got %q", got)
	}

	entry.metaMu.RLock()
	gotExpiry := entry.expiresAt
	entry.metaMu.RUnlock()
	if !gotExpiry.Equal(newExpiry) {
		t.Fatalf("expected expiry %v, got %v", newExpiry, gotExpiry)
	}
}

func TestSupportsSessionReauth(t *testing.T) {
	if !supportsSessionReauth(dyauth.TokenInfo{Token: "user-token", Type: dyauth.TokenTypeAuthKey}) {
		t.Fatal("expected auth key to support session reauth")
	}
	if supportsSessionReauth(dyauth.TokenInfo{Token: "api-key-token", Type: dyauth.TokenTypeApiKey}) {
		t.Fatal("expected api key to skip session reauth")
	}

	queryAPIKeyToken := "header.eyJzdWIiOiI0MmQxYzM5OS1lOGM5LTQ5MjQtOWRmYy05NTkyMTZlYTZhYzUiLCJqdGkiOiI4NTliNzUwZi00MmZiLTQ3ZmMtODkzNy0yNGUwYmZjYzQyZGMiLCJzaWQiOiI4NTliNzUwZi00MmZiLTQ3ZmMtODkzNy0yNGUwYmZjYzQyZGMiLCJ0eXBlIjoiYXBpX2tleSIsImFwaV9rZXlfaWQiOiIzNzRjMGMwZi0yN2I1LTQ4MDYtYmM4MC01NTVkYzA3ZjdlNmEiLCJhY2NvdW50X2lkIjoiNDJkMWMzOTktZThjOS00OTI0LTlkZmMtOTU5MjE2ZWE2YWM1IiwidmVyIjoiMiIsImVwb2NoIjoiMCIsIm5iZiI6MTc3OTcyOTc1OCwiZXhwIjoxNzgyMzIxNzU4LCJpc3MiOiJzb2xhci1uZXR3b3JrIiwiYXVkIjoiaHR0cHM6Ly9zb2xpYW4uYXBwIn0.signature"
	if supportsSessionReauth(dyauth.TokenInfo{Token: queryAPIKeyToken, Type: dyauth.TokenTypeAuthKey}) {
		t.Fatal("expected API key JWT to skip session reauth even when extracted as auth key")
	}
}
