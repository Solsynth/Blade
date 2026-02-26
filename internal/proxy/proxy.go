package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/dysonnetwork/gateway/internal/config"
	"github.com/dysonnetwork/gateway/internal/logging"
	"github.com/gin-gonic/gin"
)

type Proxy struct {
	serviceURLs map[string]string
}

func New(cfg *config.Config) *Proxy {
	serviceURLs := make(map[string]string)
	for _, name := range cfg.Endpoints.ServiceNames {
		url := config.GetServiceHTTP(name)
		if url != "" {
			serviceURLs[name] = url
		}
	}

	return &Proxy{
		serviceURLs: serviceURLs,
	}
}

func (p *Proxy) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		if path == "/ws" || strings.HasPrefix(path, "/ws/") {
			p.handleWebSocket(c, "ring")
			return
		}

		if strings.HasPrefix(path, "/.well-known/") {
			p.handleWellKnown(c)
			return
		}

		if strings.HasPrefix(path, "/activitypub") || strings.HasPrefix(path, "/api/activitypub") {
			p.handleProxy(c, "sphere", "/activitypub")
			return
		}

		if strings.HasPrefix(path, "/swagger/") {
			parts := strings.SplitN(path[1:], "/", 3)
			if len(parts) >= 2 {
				serviceName := parts[1]
				if _, ok := p.serviceURLs[serviceName]; ok {
					newPath := "/swagger/" + strings.Join(parts[2:], "/")
					p.handleProxyWithPath(c, serviceName, newPath)
					return
				}
			}
		}

		parts := strings.SplitN(path[1:], "/", 2)
		if len(parts) > 0 {
			serviceName := parts[0]
			if _, ok := p.serviceURLs[serviceName]; ok {
				var newPath string
				if len(parts) > 1 {
					newPath = "/api/" + parts[1]
				} else {
					newPath = "/api"
				}
				p.handleProxyWithPath(c, serviceName, newPath)
				return
			}
		}

		c.JSON(http.StatusNotFound, gin.H{
			"error": "route not found",
			"code":  "ROUTE_NOT_FOUND",
		})
	}
}

func (p *Proxy) handleWebSocket(c *gin.Context, serviceName string) {
	p.handleProxy(c, serviceName, "")
}

func (p *Proxy) handleWellKnown(c *gin.Context) {
	path := c.Request.URL.Path

	switch path {
	case "/.well-known/openid-configuration":
		p.handleProxy(c, "pass", "/.well-known/openid-configuration")
	case "/.well-known/jwks":
		p.handleProxy(c, "pass", "/.well-known/jwks")
	case "/.well-known/webfinger":
		p.handleProxy(c, "sphere", "/.well-known/webfinger")
	default:
		c.JSON(http.StatusNotFound, gin.H{
			"error": "endpoint not found",
			"code":  "ENDPOINT_NOT_FOUND",
		})
	}
}

func (p *Proxy) handleProxy(c *gin.Context, serviceName string, pathOverride string) {
	baseURL := p.serviceURLs[serviceName]
	if baseURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "service not available",
			"code":  "SERVICE_UNAVAILABLE",
		})
		return
	}

	target := baseURL
	if pathOverride != "" {
		target = target + pathOverride
	} else {
		target = target + c.Request.URL.Path
	}

	p.proxyRequest(c, target)
}

func (p *Proxy) handleProxyWithPath(c *gin.Context, serviceName string, newPath string) {
	baseURL := p.serviceURLs[serviceName]
	if baseURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "service not available",
			"code":  "SERVICE_UNAVAILABLE",
		})
		return
	}

	target := baseURL + newPath
	p.proxyRequest(c, target)
}

func (p *Proxy) proxyRequest(c *gin.Context, target string) {
	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(target, "http://")

		if !strings.Contains(req.URL.Host, ":") {
			if idx := strings.Index(req.URL.Host, "/"); idx != -1 {
				req.URL.Host = req.URL.Host[:idx]
			}
		}

		originalPath := req.URL.Path
		if idx := strings.Index(target, req.URL.Host); idx != -1 {
			remaining := target[idx+len(req.URL.Host):]
			if remaining != "" && remaining != "/" {
				req.URL.Path = remaining
				if strings.Contains(req.URL.Path, "?") {
					req.URL.Path = strings.Split(req.URL.Path, "?")[0]
				}
			}
		}

		req.Host = req.URL.Host

		logging.Log.Debug().
			Str("original", originalPath).
			Str("target", req.URL.Path).
			Str("host", req.URL.Host).
			Msg("Proxying request")
	}

	proxy := &httputil.ReverseProxy{
		Director: director,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 30 * 1000000000,
			}).DialContext,
		},
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}
