# WebSocket Gateway — Multi-Tenant Namespaces

The wsgateway supports namespace-based multi-tenancy, allowing isolated connection scopes within a single gateway instance. Each namespace maintains its own connection registry, online presence, and per-namespace configuration overrides.

## Concepts

- **Namespace** — a string identifier that scopes all connection, presence, and push operations. Two clients connecting with the same `accountID` + `deviceID` in different namespaces are treated as completely independent connections.
- **Default namespace** — when a caller does not specify a namespace (empty string), the service falls back to `Config.DefaultNamespace` (defaults to `"_default"`). This ensures full backwards compatibility.

## What is isolated per namespace

| Concern | Scope |
|---|---|
| Connection registry (`connectionKey`) | namespace + accountID + deviceID |
| Online presence (Redis keys & local map) | namespace |
| Push/broadcast (`SendPacketToAccount`, `SendPacketToDevice`) | namespace |
| Session revocation disconnect | namespace |
| Debug/query endpoints | namespace |

## What is NOT isolated

- Auth: all namespaces share the same token authenticator.
- Event publishing (NATS `connect`/`disconnect` events): namespace is not part of the event subject — these are still account/device scoped.
- NATS push subject: all namespaces publish/subscribe on the same NATS subject. The namespace is embedded in the event payload.

## Configuration

```go
wsgateway.Config{
    KeepAliveInterval: 60 * time.Second, // fallback for namespaces without override
    MaxMessageBytes:   4 * 1024,
    AllowedDeviceAlt:  map[string]struct{}{"watch": {}},
    DefaultNamespace:  "_default",
    Namespaces: map[string]wsgateway.NamespaceConfig{
        "_default": {
            KeepAliveInterval: 60 * time.Second,
            MaxMessageBytes:   4 * 1024,
            AllowedDeviceAlt:  map[string]struct{}{"watch": {}},
        },
        "mobile-app": {
            KeepAliveInterval: 30 * time.Second,
            MaxMessageBytes:   8 * 1024,
            AllowedDeviceAlt:  map[string]struct{}{},
        },
    },
}
```

Per-namespace config fields override the top-level fallback. Any field left at zero value inherits the top-level default.

### Config fields

| Field | Description |
|---|---|
| `KeepAliveInterval` | Presence refresh interval. Affects how long a stale connection survives without a heartbeat. |
| `MaxMessageBytes` | Maximum inbound packet size for connections in this namespace. |
| `AllowedDeviceAlt` | Allowed `deviceAlt` query param values for HTTP connections in this namespace. |

## HTTP Connection

Clients specify namespace via the `namespace` query parameter:

```
GET /ws?namespace=mobile-app
GET /ws                          # uses DefaultNamespace
GET /ws?namespace=               # uses DefaultNamespace
```

The namespace is validated at connection time and stored on the connection for its lifetime.

## Redis Presence Keys

The key structure changed to include namespace:

```
# Before
{prefix}:account:{accountID}
{prefix}:device:{deviceID}
{prefix}:accounts

# After
{prefix}:ns:{namespace}:account:{accountID}
{prefix}:ns:{namespace}:device:{deviceID}
{prefix}:ns:{namespace}:accounts
```

**Migration note**: Existing presence data in Redis will not be visible after upgrade. Connections will re-register under the new keys naturally as clients reconnect. No manual migration is needed.

## API Changes

All presence, query, and push methods now take a `namespace` parameter as the first argument. Pass empty string `""` to use the default namespace.

### Service methods

```go
// Connection management
svc.TryAdd(auth, namespace, deviceID, conn, cancel)
svc.TryAddUniqueDevice(namespace, account, deviceID, conn)
svc.HandleConnection(ctx, auth, namespace, deviceID, conn)
svc.Disconnect(namespace, accountID, deviceID, reason)
svc.DisconnectSession(namespace, sessionID, accountID, deviceID, reason)

// Online presence
svc.GetDeviceIsConnected(namespace, deviceID) bool
svc.GetAccountIsConnected(namespace, accountID) bool
svc.GetConnectedDeviceIDs(namespace, deviceIDs) []string
svc.GetAllConnectedUserIDs(namespace) []string
svc.GetAllConnectedDeviceIDs(namespace) []string
svc.GetDevicesByAccount(namespace, accountID) []string
svc.GetAccountsByDevice(namespace, deviceID) []string

// Push (namespace-scoped)
svc.SendPacketToAccount(namespace, accountID, packet)
svc.SendPacketToAccountExcept(namespace, accountID, packet, excludedDeviceIDs)
svc.SendPacketToDevice(namespace, deviceID, packet)

// Cross-namespace
svc.GetConnectionSnapshots() []ConnectionSnapshot  // includes namespace field
```

### PresenceStore interface

```go
type PresenceStore interface {
    Register(ctx, namespace, accountID, deviceID, connectionID string) error
    Refresh(ctx, namespace, accountID, deviceID, connectionID string) error
    Remove(ctx, namespace, accountID, deviceID, connectionID string) error
    AccountConnected(ctx, namespace, accountID string) (bool, error)
    DeviceConnected(ctx, namespace, deviceID string) (bool, error)
    DevicesConnected(ctx, namespace string, deviceIDs []string) (map[string]bool, error)
}
```

### PushPublisher interface (NATS)

```go
type PushPublisher interface {
    PublishAccount(ctx, namespace, accountID string, packet, excluded) error
    PublishDevices(ctx, namespace string, deviceIDs []string, packet) error
}
```

The NATS push event payload now includes a `namespace` field. When subscribing to pushes, the namespace is resolved from the event and used to route to the correct connection scope. Empty namespace in the event falls back to the service's default namespace.

### gRPC

All gRPC methods pass empty namespace `""` (default) for backwards compatibility. The proto definitions are unchanged — namespace support at the gRPC layer is not yet exposed.

### Debug endpoints

Debug endpoints accept an optional `namespace` query parameter:

```
GET /debug/ws/summary?namespace=mobile-app
GET /debug/ws/account/{accountId}?namespace=mobile-app
GET /debug/ws/device/{deviceId}?namespace=mobile-app
```

`/debug/ws/summary` and `/debug/ws/connections` include namespace information in their response. `GetConnectionSnapshots()` returns snapshots across all namespaces.

## Use cases

### Multiple product lines on one gateway

Different client apps (e.g., web app vs mobile app) can use separate namespaces to avoid device ID collisions while sharing the same gateway infrastructure. Each namespace can have its own keepalive interval and message size limits tuned for its client profile.

### Per-namespace online observation

Different tenants may have different SLA requirements for online detection. Shorter `KeepAliveInterval` for real-time namespaces, longer intervals for less latency-sensitive ones.

### Gradual rollout

Deploy a new namespace alongside the default to test configuration changes without affecting existing clients. Once validated, migrate clients by having them pass the new namespace in their connection URL.
