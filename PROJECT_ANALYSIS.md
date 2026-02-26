# DysonNetwork.Gateway - Project Analysis

## Overview

This is an **API Gateway** built on **YARP (Yet Another Reverse Proxy)** for a distributed microservices architecture called "Dyson Network" (also known as Solar Network). It serves as the single entry point for multiple backend microservices.

## Primary Function

- Reverse proxy routing requests to backend microservices
- Health monitoring and readiness gating
- Rate limiting for API protection
- Service discovery integration
- OpenID/JWT authentication support (pass-through)
- WebSocket support (for real-time features)
- ActivityPub protocol support (for Fediverse integration)

---

## Architecture Overview

### Microservices in the Ecosystem

| Service | Purpose |
|---------|---------|
| **DysonNetwork.Ring** | Core identity/auth service |
| **DysonNetwork.Pass** | Authentication service |
| **DysonNetwork.Drive** | File storage service |
| **DysonNetwork.Sphere** | Social/fediverse service |
| **DysonNetwork.Develop** | Developer services |
| **DysonNetwork.Insight** | Analytics |
| **DysonNetwork.Zone** | Geo/location services |
| **DysonNetwork.Messager** | Messaging |
| **DysonNetwork.Wallet** | Payment/wallet |
| **DysonNetwork.Control** | Admin control |
| **DysonNetwork.Fitness** | Health/fitness tracking |

### Core Components

| Component | File | Purpose |
|-----------|------|---------|
| **YARP Reverse Proxy** | Program.cs | Routes HTTP requests to backend services |
| **Health Aggregator** | GatewayHealthAggregator.cs | Background service polling each service's `/health` endpoint every 5-10 seconds |
| **Readiness Store** | GatewayReadinessStore.cs | Thread-safe store tracking service health states |
| **Readiness Middleware** | GatewayReadinessMiddleware.cs | Returns 503 if core services are unhealthy |
| **Rate Limiting** | Program.cs | 120 requests/minute/IP with burst allowance |

### Request Flow

```
Client Request
    ↓
Rate Limiter (120 req/min/IP)
    ↓
GatewayReadinessMiddleware (blocks if core services unhealthy)
    ↓
YARP Reverse Proxy (routes based on path)
    ↓
Backend Service (ring, pass, drive, sphere, etc.)
```

---

## Technology Stack

### .NET Version
- **.NET 10.0** (latest preview)
- Target Framework: `net10.0`

### Key Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| **Yarp.ReverseProxy** | 2.3.0 | Core reverse proxy functionality |
| **Microsoft.Extensions.ServiceDiscovery.Yarp** | 10.2.0 | Dynamic service discovery |
| **Nerdbank.GitVersioning** | 3.9.50 | Version management |

### Shared Dependencies
- ASP.NET Core 10 - Web framework
- NodaTime - Date/time handling
- Entity Framework Core - Database access
- PostgreSQL (Npgsql) - Database provider
- Redis - Caching
- NATS - Message queue
- OpenTelemetry - Observability/tracing
- Polly - Resilience patterns (retry, circuit breaker)
- gRPC - Inter-service communication
- MessagePack - Binary serialization
- Swashbuckle - Swagger/OpenAPI

---

## Key Features

### Routing Configuration

**Special Routes:**
- `/ws` → ring (WebSocket)
- `/.well-known/openid-configuration` → pass (OIDC discovery)
- `/.well-known/jwks` → pass (JWT keys)
- `/.well-known/webfinger` → sphere (Fediverse)
- `/activitypub/**` → sphere (ActivityPub)

**API Routes Pattern:**
- `/<serviceName>/**` → `<serviceName>` cluster
- Transform: removes `/<serviceName>` prefix, adds `/api` prefix

**Swagger Routes:**
- `/swagger/<serviceName>/**` → `<serviceName>` cluster

### Health Monitoring
- Active health checks every 10 seconds
- Passive health tracking (on request failures)
- 5-second check interval for aggregator
- Core services: ring, pass, drive, sphere (configurable)

### Rate Limiting
- Fixed window: 120 requests per minute per IP
- Burst allowance: 10 queued requests
- Returns 429 Too Many Requests when exceeded

### CORS
- Allows all origins
- Exposes custom headers: `X-Total`, `X-NotReady`

---

## Performance Characteristics

### Async Patterns
- Fully async - Uses `async/await` throughout
- BackgroundService for health aggregation (non-blocking)
- Task.Delay for polling intervals

### Concurrency Handling
```csharp
// GatewayReadinessStore.cs
private readonly Lock _lock = new();  // System.Threading.Lock (.NET 10)

// Thread-safe updates
lock (_lock)
{
    _services[state.ServiceName] = state;
    RecalculateLocked();
}
```

### Configuration
- Kestrel configured for large request bodies (`long.MaxValue`)
- HTTP/1.1 and HTTP/2 support
- Forwarded headers enabled for proxy/load balancer scenarios

### Performance Considerations
- **Minimal computation** - Gateway is mostly pass-through
- **Network-bound** - Latency dominated by backend service calls
- **Memory footprint** - Low (no heavy in-memory caching)
- **Lock contention** - Single lock in readiness store (low contention)

---

## Configuration

### appsettings.json
```json
{
  "Endpoints": {
    "ServiceNames": ["ring", "pass", "drive", "sphere", "develop", "insight", "zone", "messager"],
    "CoreServiceNames": ["ring", "pass", "drive", "sphere"]
  },
  "Cache": { "Serializer": "JSON" },
  "SiteUrl": "http://localhost:3000"
}
```

### Environment Variables
- `HTTP_PORTS` - Comma-separated ports (default: 6000)
- `GRPC_PORT` - gRPC port (default: 5001)
- `ASPNETCORE_ENVIRONMENT` - Environment (Development/Production)

### Docker
- Base image: `mcr.microsoft.com/dotnet/aspnet:10.0`
- Exposes ports 8080, 8081

---

## Porting Recommendations

### If porting to **Rust**:
- Use **Axum** or **Actix-web** for HTTP routing
- **Tokio** for async runtime
- Consider **Caddy** or **Nginx** for simpler reverse proxy needs
- YARP-like functionality: **Warp** or custom middleware

### If porting to **Go**:
- **Gin** or **Chi** for routing
- Built-in `httputil.ReverseProxy` is similar to YARP
- **Gorilla Mux** or **Fiber** for alternative routing

### If porting to **C++**:
- **Boost.Beast** for HTTP
- **libcurl** for proxy functionality
- Consider **C++ REST SDK** (cpprestsdk)
- Much more complex - recommend Go/Rust instead

### Key Performance Optimizations to Preserve
1. **Service discovery** - Don't hardcode backend URLs
2. **Health checking** - Essential for resilience
3. **Rate limiting** - Protect backend services
4. **Circuit breaker** - Add Polly-like pattern (in backend services)
5. **Observability** - OpenTelemetry integration is critical

---

## Summary

This is a **production-grade API Gateway** using industry-standard YARP reverse proxy. It's relatively lightweight and focused on:

- Reliable request routing
- Service health monitoring
- Basic rate limiting
- Protocol support (HTTP, WebSocket, gRPC, ActivityPub)

The gateway itself has **minimal business logic** - it's primarily a routing and health monitoring layer. The main performance considerations are network latency to backend services, not the gateway's own processing.

## Editor Notes

If you're working on a new impl of this. The service discovery part can be configured by the local config file. Since the service mostly works with k8s and docker compose. They got magic dns set up.

