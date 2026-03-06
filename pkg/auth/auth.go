package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	gen "git.solsynth.dev/sosys/spec/gen/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type AuthResult struct {
	Account *gen.DyAccount
	Session *gen.DyAuthSession
}

type TokenType string

const (
	TokenTypeUserJWT         TokenType = "UserJWT"
	TokenTypeAPIKeyJWT       TokenType = "ApiKeyJWT"
	TokenTypeLegacyUserToken TokenType = "LegacyUserToken"
	TokenTypeLegacyAPIKey    TokenType = "LegacyApiKey"
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

type GrpcAuthDialConfig struct {
	Target        string
	UseTLS        bool
	TLSSkipVerify bool
	TLSServerName string
}

func NewGrpcTokenAuthenticator(cfg GrpcAuthDialConfig) (*GrpcTokenAuthenticator, error) {
	target, useTLS := NormalizeAuthGRPCTarget(cfg.Target, cfg.UseTLS)
	if target == "" {
		return nil, errors.New("auth gRPC target is empty")
	}

	var transportCredentials credentials.TransportCredentials
	if useTLS {
		tlsCfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.TLSSkipVerify,
		}
		if cfg.TLSServerName != "" {
			tlsCfg.ServerName = cfg.TLSServerName
		}
		transportCredentials = credentials.NewTLS(tlsCfg)
	} else {
		transportCredentials = insecure.NewCredentials()
	}

	conn, err := grpc.Dial(
		target,
		grpc.WithTransportCredentials(transportCredentials),
	)
	if err != nil {
		return nil, fmt.Errorf("dial auth service: %w", err)
	}
	return &GrpcTokenAuthenticator{conn: conn}, nil
}

func NormalizeAuthGRPCTarget(rawTarget string, useTLS bool) (string, bool) {
	target := strings.TrimSpace(rawTarget)
	if target == "" {
		return "", useTLS
	}

	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" {
		return target, useTLS
	}

	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "grpc":
		return parsed.Host, false
	case "grpcs", "https":
		return parsed.Host, true
	case "http":
		return parsed.Host, false
	default:
		return target, useTLS
	}
}

func (a *GrpcTokenAuthenticator) Authenticate(ctx context.Context, tokenInfo TokenInfo, r *http.Request) (*AuthResult, error) {
	ip := ExtractIP(r)
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

func ExtractToken(r *http.Request) (TokenInfo, bool) {
	if tk := strings.TrimSpace(r.URL.Query().Get("tk")); tk != "" {
		if looksLikeJWT(tk) {
			return TokenInfo{Token: tk, Type: TokenTypeUserJWT}, true
		}
		return TokenInfo{Token: tk, Type: TokenTypeLegacyUserToken}, true
	}

	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz != "" {
		scheme, token, ok := splitAuthorizationHeader(authz)
		if ok {
			switch strings.ToLower(scheme) {
			case "bearer":
				typeName := TokenTypeUserJWT
				if !looksLikeJWT(token) {
					typeName = TokenTypeLegacyUserToken
				}
				return TokenInfo{Token: token, Type: typeName}, true
			case "bot":
				typeName := TokenTypeAPIKeyJWT
				if !looksLikeJWT(token) {
					typeName = TokenTypeLegacyAPIKey
				}
				return TokenInfo{Token: token, Type: typeName}, true
			case "atfield":
				return TokenInfo{Token: token, Type: TokenTypeLegacyUserToken}, true
			case "akfield":
				return TokenInfo{Token: token, Type: TokenTypeLegacyAPIKey}, true
			}
		}
	}

	if cookie, err := r.Cookie("AuthToken"); err == nil {
		tk := strings.TrimSpace(cookie.Value)
		if tk != "" {
			tt := TokenTypeLegacyUserToken
			if looksLikeJWT(tk) {
				tt = TokenTypeUserJWT
			}
			return TokenInfo{Token: tk, Type: tt}, true
		}
	}

	return TokenInfo{}, false
}

func AuthenticateRequest(ctx context.Context, auth TokenAuthenticator, r *http.Request) (*AuthResult, error) {
	if auth == nil {
		return nil, errors.New("token authenticator is not configured")
	}
	tokenInfo, ok := ExtractToken(r)
	if !ok || strings.TrimSpace(tokenInfo.Token) == "" {
		return nil, errors.New("no token was provided")
	}

	authCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	return auth.Authenticate(authCtx, tokenInfo, r)
}

func ExtractIP(r *http.Request) string {
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

func splitAuthorizationHeader(value string) (scheme string, token string, ok bool) {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], strings.TrimSpace(strings.Join(parts[1:], " ")), true
}

func looksLikeJWT(token string) bool {
	return strings.Count(token, ".") == 2
}
