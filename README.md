# Blade

An API Gateway built in Go using Gin, ported from the .NET YARP-based gateway.

In fact, it's not the next generation gateway, it's the prev generation.
Because Solar Network is built by pure Go at the v2, and migrated to .NET at v3, now we planned to move some core services to Go again.

## Features

- **Reverse Proxy Routing** - Routes requests to backend microservices
- **Health Monitoring** - Background health checks every 10 seconds
- **Readiness Gating** - Returns 503 if core services are unhealthy
- **CORS Support** - Allows all origins with custom headers
- **Special Routes** - Fully configurable route system via `routes`
- **Route Transforms** - Strips service prefix, adds `/api` prefix

## Configuration

Edit `configs/config.toml`:

```toml
siteUrl = "https://solian.app"

[cache]
serializer = "JSON"

[services]
ring = { http = "http://ring:5000", grpc = "ring:5001" }
pass = { http = "http://pass:5000", grpc = "pass:5001" }
drive = { http = "http://drive:5000", grpc = "drive:5001" }
sphere = { http = "http://sphere:5000", grpc = "sphere:5001" }
develop = { http = "http://develop:5000", grpc = "develop:5001" }
insight = { http = "http://insight:5000", grpc = "insight:5001" }
zone = { http = "http://zone:5000", grpc = "zone:5001" }
messager = { http = "http://messager:5000", grpc = "messager:5001" }
wallet = { http = "http://wallet:5000", grpc = "wallet:5001" }
ideask = { http = "http://ideask:5000", grpc = "ideask:5001" }

[endpoints]
serviceNames = ["ring", "pass", "drive", "sphere", "develop", "insight", "zone", "messager", "wallet", "ideask"]
coreServiceNames = ["ring", "pass", "drive", "sphere"]

[rateLimit]
requestsPerMinute = 120
burstAllowance = 10

[health]
checkIntervalSeconds = 10

[server]
port = "6000"
readTimeout = 60
writeTimeout = 60

[websocket]
enabled = true
path = "/ws"
keepAliveSeconds = 60
maxMessageBytes = 4096
allowedDeviceAlternatives = ["watch"]
```

### Environment Variables

| Variable         | Description           | Default               |
| ---------------- | --------------------- | --------------------- |
| `CONFIG_PATH`    | Path to config file   | `configs/config.toml` |
| `GIN_MODE`       | `debug` or `release`  | `debug`               |
| `ZEROLOG_PRETTY` | Enable pretty logging | `false`               |

### Special Routes Configuration

The gateway supports fully configurable special routes:

```toml
[[routes]]
path = "/.well-known/openid-configuration"
service = "pass"
target = "/auth/.well-known/openid-configuration"
prefix = false

[[routes]]
path = "/activitypub"
service = "sphere"
target = "/activitypub"
prefix = true           # true for wildcard matching
```

| Field     | Description                                                             |
| --------- | ----------------------------------------------------------------------- |
| `path`    | Source path to match (e.g., `/ws`, `/.well-known/openid-configuration`) |
| `service` | Target service name                                                     |
| `target`  | Path on the backend service                                             |
| `prefix`  | If `true`, match path as prefix (e.g., `/activitypub/**`)               |

## Build & Run

### Local

```bash
# Build
go build -o gateway ./cmd/main.go

# Run
./gateway

# Or with custom config
CONFIG_PATH=./configs/config.toml ./gateway
```

### Docker

```bash
# Build
docker build -t dyson-gateway .

# Run
docker run -p 6000:6000 dyson-gateway

# Run with custom config
docker run -p 6000:6000 -v ./config.toml:/app/configs/config.toml dyson-gateway
```

## Endpoints

| Endpoint                | Description                                                        |
| ----------------------- | ------------------------------------------------------------------ |
| `GET /health`           | Gateway health status                                              |
| `/<service>/**`         | Proxied to backend service (e.g., `/ring/**` → `ring:5000/api/**`) |
| `/ws`                   | Native WebSocket gateway (configurable via `websocket.path`)       |
| `/.well-known/*`        | .well-known endpoints (configurable via `routes`)                  |
| `/activitypub/**`       | ActivityPub (configurable via `routes`)                            |
| `/swagger/<service>/**` | Swagger docs → service                                             |

### WebSocket Authentication Notes

Current implementation follows `DysonTokenAuthHandler` behavior:

- Token extraction order: `tk` query, `Authorization` header (`Bearer`, `AtField`, `AkField`), `AuthToken` cookie
- Token validation: remote gRPC call to `DyAuthService/Authenticate` using the configured `websocket.authService` target
- Request IP is forwarded to auth service as `ip_address`

## Request Flow

```
Client Request
    ↓
Rate Limiter (120 req/min/IP)
    ↓
Readiness Middleware (503 if core services unhealthy)
    ↓
Reverse Proxy (routes based on path)
    ↓
Backend Service
```
