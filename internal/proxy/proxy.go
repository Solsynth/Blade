package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"srv.solsynth.dev/sosys/blade/internal/config"
	discovery "srv.solsynth.dev/sosys/blade/internal/discovery"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

type Proxy struct {
	serviceURLs      map[string]string
	routes           []config.RouteRule
	maintenance      config.MaintenanceConfig
	blockedSet       map[string]struct{}
	registry         *discovery.Registry
	transport        http.RoundTripper
	trustedProxyHops int
}

var defaultProxyTransport = newProxyTransport()

func newProxyTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
}

func New(cfg *config.Config, registries ...*discovery.Registry) *Proxy {
	serviceURLs := make(map[string]string)
	for _, name := range cfg.Endpoints.ServiceNames {
		url := config.GetServiceHttp(name)
		if url != "" {
			serviceURLs[name] = url
		}
	}

	p := &Proxy{
		serviceURLs:      serviceURLs,
		routes:           cfg.Routes,
		maintenance:      cfg.Maintenance,
		blockedSet:       toServiceSet(cfg.Maintenance.Services),
		transport:        newProxyTransport(),
		trustedProxyHops: cfg.Proxy.TrustedProxyHops,
	}
	if len(registries) > 0 {
		p.registry = registries[0]
	}
	return p
}

func (p *Proxy) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		if p.isFullMaintenance() {
			p.respondMaintenanceBlocked(c, "")
			return
		}

		// Check special routes
		for _, route := range p.routes {
			matched := false
			if route.Prefix {
				matched = strings.HasPrefix(path, route.Path)
			} else {
				matched = path == route.Path || strings.HasPrefix(path, route.Path+"/")
			}

			if matched {
				if p.isServiceBlocked(route.Service) {
					p.respondMaintenanceBlocked(c, route.Service)
					return
				}
				p.handleSpecialRoute(c, route)
				return
			}
		}

		// Swagger route
		if strings.HasPrefix(path, "/swagger/") {
			parts := strings.SplitN(path[1:], "/", 3)
			if len(parts) >= 2 {
				serviceName := parts[1]
				if p.hasService(c, serviceName) {
					if p.isServiceBlocked(serviceName) {
						p.respondMaintenanceBlocked(c, serviceName)
						return
					}
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
			if p.hasService(c, serviceName) {
				if p.isServiceBlocked(serviceName) {
					p.respondMaintenanceBlocked(c, serviceName)
					return
				}
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

// hasService admits both statically configured services and services that
// exist only in the discovery registry. This lets a newly registered service
// become routable without a Blade configuration deployment.
func (p *Proxy) hasService(c *gin.Context, serviceName string) bool {
	if _, ok := p.serviceURLs[serviceName]; ok {
		return true
	}
	if p.registry == nil {
		return false
	}
	instances, err := p.registry.List(c.Request.Context(), serviceName)
	return err == nil && len(instances) > 0
}

func toServiceSet(services []string) map[string]struct{} {
	serviceSet := make(map[string]struct{}, len(services))
	for _, svc := range services {
		if svc == "" {
			continue
		}
		serviceSet[strings.ToLower(svc)] = struct{}{}
	}
	return serviceSet
}

func (p *Proxy) isFullMaintenance() bool {
	if !p.maintenance.Enabled {
		return false
	}
	return strings.EqualFold(p.maintenance.Mode, "full")
}

func (p *Proxy) isServiceBlocked(serviceName string) bool {
	if !p.maintenance.Enabled {
		return false
	}
	if p.isFullMaintenance() {
		return true
	}
	if !strings.EqualFold(p.maintenance.Mode, "service") {
		return false
	}
	_, blocked := p.blockedSet[strings.ToLower(serviceName)]
	return blocked
}

func (p *Proxy) respondMaintenanceBlocked(c *gin.Context, serviceName string) {
	logEvent := logging.Log.Warn().Str("path", c.Request.URL.Path).Str("mode", p.maintenance.Mode)
	if serviceName != "" {
		logEvent = logEvent.Str("service", serviceName)
	}
	logEvent.Msg("Request blocked by maintenance mode")

	resp := gin.H{
		"error": "service under maintenance",
		"code":  "MAINTENANCE_MODE",
	}
	if serviceName != "" {
		resp["service"] = serviceName
	}
	c.JSON(http.StatusServiceUnavailable, resp)
}

func (p *Proxy) handleSpecialRoute(c *gin.Context, route config.RouteRule) {
	baseURL := p.serviceURL(c, route.Service)
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
	baseURL := p.serviceURL(c, serviceName)
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
	baseURL := p.serviceURL(c, serviceName)
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

// serviceURL prefers a healthy registered instance and keeps the configured
// target as a migration-safe fallback for services not yet self-registering.
func (p *Proxy) serviceURL(c *gin.Context, serviceName string) string {
	if p.registry != nil {
		instances, err := p.registry.List(c.Request.Context(), serviceName)
		if err == nil && len(instances) > 0 {
			if endpoint, ok := p.registry.ResolveHTTP(c.Request.Context(), serviceName); ok {
				return endpoint
			}
			// A service that has registered owns its routing state. Do not send
			// traffic back to a static endpoint while all registered instances
			// are unhealthy.
			return ""
		}
	}
	return p.serviceURLs[serviceName]
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

	// Resolve the client address and the original request origin from the
	// connection (plus the trusted edge in front of the gateway). Client-supplied
	// values are never trusted: the ReverseProxy's Rewrite hook drops the
	// inbound forwarding headers and rebuilds them below.
	clientIP := ResolveClientIP(c.Request, p.trustedProxyHops)
	scheme := requestScheme(c.Request, p.trustedProxyHops)
	host := requestHost(c.Request, p.trustedProxyHops)

	rewrite := func(pr *httputil.ProxyRequest) {
		req := pr.Out
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

		stripClientTrustHeaders(req.Header)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Forwarded-Proto", scheme)
		req.Header.Set("X-Forwarded-Host", host)

		logging.Log.Debug().
			Str("original", originalPath).
			Str("target", req.URL.Path).
			Str("query", req.URL.RawQuery).
			Str("host", req.URL.Host).
			Msg("Proxying request")
	}

	transport := p.transport
	if transport == nil {
		transport = defaultProxyTransport
	}

	proxy := &httputil.ReverseProxy{
		Rewrite:   rewrite,
		Transport: transport,
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

// clientTrustHeaderPrefixes are the header namespaces that only the gateway may
// populate. A backend service must never read them from a client, so every
// forwarded request has them stripped before the gateway sets its own.
//
// X-Forwarded-* is included because a backend that trusts it (directly or
// through a library) would otherwise accept a spoofed client address or
// scheme; the gateway rebuilds the three headers it forwards from the
// connection.
var clientTrustHeaderPrefixes = []string{
	"x-account-",
	"x-user-",
	"x-auth-",
	"x-real-ip",
	"x-forwarded-",
}

// stripClientTrustHeaders removes every header a client could use to forge an
// identity or a forwarding chain. Comparison is case-insensitive because the
// canonical form of a few of these (X-Real-IP, X-Forwarded-For) is not the
// literal spelling.
func stripClientTrustHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		for _, prefix := range clientTrustHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				header.Del(name)
				break
			}
		}
	}
}

// ResolveClientIP returns the client address attributable to the request. With
// no trusted proxy in front (trustedProxyHops == 0) that is the peer that
// opened the connection; otherwise it is read from X-Forwarded-For, skipping
// the trustedProxyHops proxies that appended to it, so the value is one the
// outermost trusted proxy observed rather than one a client supplied.
//
// It is the single source of truth for the client address: both the forwarded
// headers and the access log use it, so a spoofed X-Forwarded-For can never
// reach a backend or the audit trail.
func ResolveClientIP(r *http.Request, trustedProxyHops int) string {
	if trustedProxyHops > 0 {
		if client := forwardedClientIP(r.Header.Get("X-Forwarded-For"), trustedProxyHops); client != "" {
			return client
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// forwardedClientIP returns the address recorded trustedProxyHops entries from
// the right of an X-Forwarded-For value. Each trusted proxy appends the peer it
// saw, so that position holds what the outermost trusted proxy observed; every
// entry to its left is client-supplied and ignored. It returns "" when the
// header does not carry enough entries to reach a trusted position.
func forwardedClientIP(value string, trustedProxyHops int) string {
	if trustedProxyHops < 1 {
		return ""
	}
	entries := make([]string, 0, 8)
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	if len(entries) < trustedProxyHops {
		return ""
	}
	return entries[len(entries)-trustedProxyHops]
}

// requestScheme is the scheme the client used to reach the gateway. It comes
// from the trusted edge when one is configured, and from the connection
// otherwise, never from an unvalidated header.
func requestScheme(r *http.Request, trustedProxyHops int) string {
	if trustedProxyHops > 0 {
		if proto := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			return proto
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// requestHost is the host the client dialled, taken from the trusted edge when
// one is configured and from the request line otherwise.
func requestHost(r *http.Request, trustedProxyHops int) string {
	if trustedProxyHops > 0 {
		if host := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); host != "" {
			return host
		}
	}
	return r.Host
}

// firstHeaderValue returns the leftmost comma-separated value of a header,
// which is the one the outermost proxy wrote.
func firstHeaderValue(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(value, ",")[0])
}
