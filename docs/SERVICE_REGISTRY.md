# Service Registry Behavior

Blade derives its runtime service registry from two sources:

1. Static service entries in Blade configuration.
2. Live instances registered through `DyServiceDiscoveryService` over gRPC.

The effective registry is their union. This allows a service to be introduced
through gRPC registration without a Blade configuration deployment, while
preserving static entries during migration.

## Precedence

Registration takes precedence over configuration for the same service name.

- If a service has one or more live registered instances, Blade uses the
  registered instances as the source of endpoint and health information.
- If all registered instances are unhealthy, Blade does not send traffic to the
  configured HTTP endpoint.
- The configured HTTP endpoint is used only when the service has no registered
  instances.
- A service known only through registration is included in health state and is
  available for proxy routing once a healthy registered HTTP instance exists.

Service names are normalized by the registry, so registration names should be
treated as lowercase.

## Endpoint map

Register protocol endpoints in `endpoints`, a `map<string, string>`. Blade
uses `http` for HTTP proxying and health checks, and `grpc` for capabilities.
Additional protocol keys (for example, `metrics`, `nats`, or `ws`) are stored
and returned through discovery for consumers that understand them.

When an `http` or `grpc` map entry is present, it overrides the corresponding
legacy `http_endpoint` or `grpc_endpoint` field. Legacy fields remain accepted
for existing clients.

## Relay catalog

L4 relay nodes (`cmd/relay`) are deployed outside the cluster network, so they
are the one service that does not register over the internal gRPC API. They
reach the gateway's public HTTPS entry instead:

| Method | Path | Purpose |
| --- | --- | --- |
| `PUT` | `/relays/{id}` | Register or renew, carrying the relay's own health report |
| `DELETE` | `/relays/{id}` | Withdraw before the lease ends |
| `GET` | `/relays` | Public client-facing catalog: the host, port, region, weight, and health clients dial and sort by |

The control plane (`PUT`/`DELETE`) requires
`authorization: Bearer <discovery.registrationToken>`; `GET /relays` is
deliberately anonymous because it is the client's relay-selection contract.
Registering an instance publishes a single `tcp` endpoint — the `host:port`
clients dial — plus `region` in `metadata`, and uses `weight` for client-side
selection. The
service name, lease, and path belong to the gateway
(`discovery.relayServiceName`, default `relay`), so a relay cannot announce
itself as some other service.

Those control routes are mounted ahead of the readiness gate: relays must be
able to register and renew while core services are unhealthy.

Because the elected checker cannot reach an out-of-network relay, the relay's
heartbeat *is* its health: `healthy` travels with the lease, and a relay that
stops reporting expires out of the catalog. The checker skips
`discovery.relayServiceName` entirely, so a probe can never overwrite a
self-report.

Blade exposes those instances at the public, client-facing `GET /relays`, built
from the registry service named by `discovery.relayServiceName`:

```json
{"relays":[{"id":"jp-01","endpoint":"relay-jp.solian.app","port":443,"region":"jp","weight":1,"healthy":true}]}
```

`endpoint` is the host only; clients append `port`. Instances without a
parsable `tcp` endpoint are skipped. The endpoint returns `503` when discovery
is disabled.

## Health checks and readiness

One Redis-elected Blade replica probes each registered HTTP endpoint at
`<http_endpoint>/health`, except for services that report their own health
(relays, see above). The probe result is written back to the registry; all
Blade replicas then consume that shared health state.

Registered-only services are health-checked even if absent from
`endpoints.serviceNames`. A registered-only service with no current instance,
or with no healthy instance, is represented as unhealthy. Static services keep
their local HTTP health-check fallback only while no instance is registered.

`endpoints.coreServiceNames` still controls readiness gating. Adding a
registered service does not automatically make it a core dependency; add it to
that configuration list when Blade readiness must depend on it.

## Capabilities

The capabilities document is built from the discovery registry. Blade queries
one healthy registered gRPC endpoint for each service. A gRPC
`Unimplemented` response from the capabilities method means that service does
not support capabilities yet; Blade ignores that response rather than marking
the full document incomplete. Other discovery, availability, and query
failures still make the document incomplete.

## Registration logging

Every successful `Register` RPC emits an info-level log containing the service
name, instance ID, advertised HTTP endpoint, advertised gRPC endpoint, and
lease expiry. The registration credential is never logged.

For client setup, lease renewal, and authentication details, see
[SERVICE_DISCOVERY_CLIENT.md](SERVICE_DISCOVERY_CLIENT.md).
