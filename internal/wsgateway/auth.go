package wsgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	gen "git.solsynth.dev/solarnetwork/dysonproto/gen/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type AuthResult struct {
	Account *gen.DyAccount
	Session *gen.DyAuthSession
}

type TokenType string

const (
	TokenTypeAuthKey TokenType = "AuthKey"
	TokenTypeOidcKey TokenType = "OidcKey"
	TokenTypeAPIKey  TokenType = "ApiKey"
)

type TokenInfo struct {
	Token string
	Type  TokenType
}

type TokenAuthenticator interface {
	Authenticate(ctx context.Context, tokenInfo TokenInfo, r *http.Request) (*AuthResult, error)
}

type GrpcTokenAuthenticator struct {
	conn *grpc.ClientConn
}

func NewGrpcTokenAuthenticator(target string) (*GrpcTokenAuthenticator, error) {
	conn, err := grpc.Dial(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial auth service: %w", err)
	}
	return &GrpcTokenAuthenticator{conn: conn}, nil
}

func (a *GrpcTokenAuthenticator) Authenticate(ctx context.Context, tokenInfo TokenInfo, r *http.Request) (*AuthResult, error) {
	ip := extractIP(r)
	req := &gen.DyAuthenticateRequest{Token: tokenInfo.Token}
	if ip != "" {
		req.IpAddress = wrapperspb.String(ip)
	}

	client := gen.NewDyAuthServiceClient(a.conn)

	if resp, err := client.Authenticate(ctx, req); err != nil {
		return nil, err
	} else if !resp.GetValid() {
		msg := strings.TrimSpace(resp.GetMessage())
		if msg == "" {
			msg = "token is not valid"
		}
		return nil, errors.New(msg)
	} else if resp.GetSession() == nil || resp.GetSession().GetAccount() == nil {
		return nil, errors.New("session not found")
	} else {
		return &AuthResult{Account: resp.GetSession().GetAccount(), Session: resp.GetSession()}, nil
	}
}

func extractToken(r *http.Request) (TokenInfo, bool) {
	if tk := strings.TrimSpace(r.URL.Query().Get("tk")); tk != "" {
		return TokenInfo{Token: tk, Type: TokenTypeAuthKey}, true
	}

	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz != "" {
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token := strings.TrimSpace(authz[len("Bearer "):])
			if strings.Count(token, ".") == 2 {
				return TokenInfo{Token: token, Type: TokenTypeOidcKey}, true
			}
			return TokenInfo{Token: token, Type: TokenTypeAuthKey}, true
		}
		if strings.HasPrefix(strings.ToLower(authz), "atfield ") {
			return TokenInfo{Token: strings.TrimSpace(authz[len("AtField "):]), Type: TokenTypeAuthKey}, true
		}
		if strings.HasPrefix(strings.ToLower(authz), "akfield ") {
			return TokenInfo{Token: strings.TrimSpace(authz[len("AkField "):]), Type: TokenTypeAPIKey}, true
		}
	}

	if cookie, err := r.Cookie("AuthToken"); err == nil {
		tk := strings.TrimSpace(cookie.Value)
		tt := TokenTypeAuthKey
		if strings.Count(tk, ".") == 2 {
			tt = TokenTypeOidcKey
		}
		return TokenInfo{Token: tk, Type: tt}, true
	}

	return TokenInfo{}, false
}

func extractIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}

	return strings.TrimSpace(r.RemoteAddr)
}

func authenticateRequest(ctx context.Context, auth TokenAuthenticator, r *http.Request) (*AuthResult, error) {
	tokenInfo, ok := extractToken(r)
	if !ok || strings.TrimSpace(tokenInfo.Token) == "" {
		return nil, errors.New("no token was provided")
	}

	authCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	return auth.Authenticate(authCtx, tokenInfo, r)
}
