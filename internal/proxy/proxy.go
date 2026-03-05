package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"git.solsynth.dev/sosys/blade/internal/config"
	"git.solsynth.dev/sosys/blade/internal/logging"
	"github.com/gin-gonic/gin"
)

type Proxy struct {
	serviceURLs map[string]string
	routes      []config.RouteRule
}

func New(cfg *config.Config) *Proxy {
	serviceURLs := make(map[string]string)
	for _, name := range cfg.Endpoints.ServiceNames {
		url := config.GetServiceHttp(name)
		if url != "" {
			serviceURLs[name] = url
		}
	}

	return &Proxy{
		serviceURLs: serviceURLs,
		routes:      cfg.Routes,
	}
}

func (p *Proxy) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// Check special routes
		for _, route := range p.routes {
			matched := false
			if route.Prefix {
				matched = strings.HasPrefix(path, route.Path)
			} else {
				matched = path == route.Path || strings.HasPrefix(path, route.Path+"/")
			}

			if matched {
				p.handleSpecialRoute(c, route)
				return
			}
		}

		// Swagger route
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

		// Default service routing
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

func (p *Proxy) handleSpecialRoute(c *gin.Context, route config.RouteRule) {
	baseURL := p.serviceURLs[route.Service]
	if baseURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "service not available",
			"code":  "SERVICE_UNAVAILABLE",
		})
		return
	}

	target := baseURL + route.Target
	if route.Prefix {
		// Preserve the rest of the path after the prefix
		path := c.Request.URL.Path
		suffix := strings.TrimPrefix(path, route.Path)
		target = baseURL + route.Target + suffix
	}

	p.proxyRequest(c, target)
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
	targetURL, err := url.Parse(target)
	if err != nil || targetURL.Host == "" {
		logging.Log.Error().
			Err(err).
			Str("target", target).
			Msg("Invalid proxy target URL")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "invalid upstream target",
			"code":  "UPSTREAM_TARGET_INVALID",
		})
		return
	}

	director := func(req *http.Request) {
		originalPath := req.URL.Path

		req.URL.Scheme = targetURL.Scheme
		if req.URL.Scheme == "" {
			req.URL.Scheme = "http"
		}
		req.URL.Host = targetURL.Host
		req.URL.Path = targetURL.Path
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		req.URL.RawPath = targetURL.RawPath

		if targetURL.RawQuery != "" {
			if req.URL.RawQuery != "" {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery
			}
		}

		req.Host = req.URL.Host

		logging.Log.Debug().
			Str("original", originalPath).
			Str("target", req.URL.Path).
			Str("query", req.URL.RawQuery).
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
