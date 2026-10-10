# Relay Node Deployment

How to deploy `cmd/relay`, the L4 relay node that fronts the API fleet.

The relay terminates no TLS. It reads the cleartext TLS ClientHello, matches the
SNI against a static allowlist, and copies bytes verbatim, so certificates and
client certificates stay end-to-end between the client and the origin. It never
sees plaintext, applies no bandwidth pacing, and does no wildcard matching.

## Build

```bash
go build -o relay ./cmd/relay                       # local
docker build -f Dockerfile.relay -t blade-relay .   # image (CI publishes blade-relay)
```

The relay binary needs no CGO and no configuration file baked in; the image
ships `configs/relay.toml` as a starting point.

## Configuration

One TOML file, path from `RELAY_CONFIG_PATH` (default `configs/relay.toml`).

### `[relay]`

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `":443"` | TCP listen address for relayed traffic |
| `id` | hostname | Instance id in the registry; must be unique per service |
| `publicHost` | `""` | Hostname clients should dial |
| `publicPort` | `443` | Port clients should dial (1..65535) |
| `region` | `""` | Region label published to clients (lowercased) |
| `weight` | `1` | Client-side selection weight (≥1) |
| `maxConnections` | `0` | In-flight cap; `0` is unlimited |
| `sniTimeout` | `"5s"` | Budget for reading the ClientHello |
| `dialTimeout` | `"5s"` | Budget for connecting to the origin |
| `idleTimeout` | `"0s"` | Drop a silent connection after this; `0` disables |
| `maxClientHelloBytes` | `8192` | Hard cap on peeked ClientHello bytes |
| `defaultUpstream` | `""` | Catch-all `host:port`; empty means reject unlisted SNI |
| `upstreams` | `[]` | Allowlist rules, `{ sni, target }` |

Rules for the allowlist:

- `target` must be `host:port` with an explicit port; there is no implicit 443,
  and the port must be 1..65535.
- `sni` is matched case-insensitively with trailing dots ignored. Duplicate
  rules after that normalization are a startup error.
- At least one rule or `defaultUpstream` is required.
- A target must not resolve back to this relay's own public address, or traffic
  loops through the relay.

### `[discovery]`

Relays are deployed outside the cluster network, so they cannot reach the
internal gRPC discovery service and the gateway cannot dial them. The control
plane therefore runs over the gateway's **public HTTPS entry**: the relay
registers, renews, and reports its own health with an authenticated `PUT`.

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Announce this node; when false the node serves traffic but is never listed |
| `url` | `""` | Base URL of the gateway's public entry, e.g. `https://api.solian.app` |
| `tlsSkipVerify` | `false` | Skip verification (self-signed fleets only) |
| `registrationToken` | `""` | Required; sent as `authorization: Bearer <token>` |

What the gateway owns, not this file: the registry service name
(`discovery.relayServiceName`, default `relay`), the lease length, and the
control path (`PUT`/`DELETE <url>/relays/<id>`). A heartbeat is an idempotent
upsert, so a changed `publicHost`, `region`, or `weight` takes effect on the
next one, and a relay that stops heartbeating simply expires out of the catalog.

### `[log]`

| Key | Default | Meaning |
| --- | --- | --- |
| `pretty` | `false` | Human-readable console output |

Environment: `RELAY_CONFIG_PATH`, `ZEROLOG_PRETTY=true`, `LOG_LEVEL=debug|info|warn|error`.

The process exits with a fatal log on an unreadable file, an unparsable value,
or any validation failure, so a bad config never deploys half-working.

## Ports and privileges

- `relay.listen` takes client traffic. Binding `:443` needs privileges: run as
  root, or grant `CAP_NET_BIND_SERVICE`, or bind `:8443` behind a port forward.
- Outbound: the relay needs egress to `discovery.url` (registration and
  heartbeats) and to every upstream target in the allowlist. Nothing else.

The node opens no other port: it has no status listener, so health and traffic
counters are read from its log (see Verification).

## Deployment

### systemd

```ini
[Unit]
Description=Blade L4 relay node
After=network-online.target

[Service]
Environment=RELAY_CONFIG_PATH=/etc/blade/relay.toml
Environment=ZEROLOG_PRETTY=true
ExecStart=/usr/local/bin/relay
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
```

### Docker

```bash
docker run -d --name blade-relay \
  -p 443:443 \
  -v /etc/blade/relay.toml:/app/configs/relay.toml:ro \
  -e RELAY_CONFIG_PATH=/app/configs/relay.toml \
  blade-relay
```

Publish only `relay.listen` externally. In containers `relay.id` must be set
explicitly: the default hostname is the container id, so replicas would
overwrite each other in the registry.

`docker-compose.relay.yml` is a working Compose version of the above, with a 5
second stop grace period; it mounts a `relay.toml` you supply.
`configs/relay-example.toml` is a fully commented starting point for that file.
The relay joins no special
network: it only needs egress to `discovery.url` and to its upstreams, so the
Compose file asks for nothing beyond the default bridge.

### Fleet notes

- One instance per `id`. Two relays on one host need distinct `id` values.
- `region` and `weight` are what clients use to pick between nodes, so fill
  them in per PoP.
- Health is the node's own report, sent with every heartbeat: the node dials
  every upstream target before each one. A node whose upstream is unreachable
  lists as `healthy: false` while it keeps heartbeating, and logs the failing
  targets once per transition.
- A crashed node disappears once its lease expires — no probe is needed to
  notice, and nothing has to be reachable from the gateway.
- Registration failures retry with exponential backoff (5s doubling to a 30s
  cap), and a failed heartbeat re-registers instead of renewing.

## Verification

```bash
# The relay's own view: it logs one counters line at startup, once a minute, and
# at shutdown, and logs "Relay upstreams unreachable" with the failing targets
# whenever that changes.
docker logs blade-relay | grep 'Relay counters'
docker logs blade-relay | grep 'Relay upstreams'

# End-to-end TLS through the node (certificate must still be the origin's)
openssl s_client -connect <publicHost>:443 -servername <allowlisted sni> </dev/null | head -20
openssl s_client -connect <publicHost>:443 -servername unlisted.example </dev/null 2>&1 | tail -3

# Listed by the master, with the health this node reports. GET /relays is the
# public, anonymous client-facing discovery contract: clients dial the returned
# host:port and sort by region, weight, and health, so it is not gated.
curl -s https://<api-host>/relays
```

A reject-by-default node aborts the unlisted-SNI handshake with no route
presented. The `Relay counters` line carries `accepted`, `rejected` (over the
connection cap), `sniRejects` (no usable ClientHello or no upstream for the
SNI), `dialErrors`, `bytesUp`, `bytesDown` (relayed bytes, excluding the peeked
ClientHello), `active`, and `uptimeSeconds`.

A node that starts, registers, and then stops heartbeating (for example because
its egress to `discovery.url` broke) drops out of the catalog on its own once
the lease expires — check the node's log for `Relay registration failed` and
`Relay lease renewal failed` lines.

## Operations

`SIGTERM` stops accepting, closes live relayed connections, withdraws the
registration, and logs a final counter line, inside a 5 second budget. Live
connections are cut rather than drained, so drains should be done by shifting
client traffic first. The lease expires on its own if the withdraw call fails.

Capacity: each relayed connection costs two goroutines and roughly 32 KiB of
copy buffer per direction, on top of the socket buffers. Size
`maxConnections` from those figures, not from bandwidth.

Common failures:

| Symptom | Cause |
| --- | --- |
| Process exits with a config error | Missing upstream rule, target without a port, duplicate SNI, or discovery enabled without `url`/token/`publicHost` |
| `Relay upstreams unreachable` in the log | The node cannot reach its own upstreams; the line names the failing targets |
| Node never appears in the catalog | Discovery disabled, rejected `registrationToken`, `discovery.url` unreachable, or the id in the path rejected by the gateway |
| Catalog entry stays `healthy: false` | The node's own upstream check is failing; look for `Relay upstreams unreachable` in its log |
| Catalog entry disappears after a while | Heartbeats stopped landing: egress to `discovery.url`, token, or gateway logs |
| Handshakes fail for one name only | That SNI has no rule and no `defaultUpstream` is set, or the client sends no cleartext SNI (ECH) |
| Connections reset under load | `maxConnections` reached; watch `rejected` in the counters line |
| Connections dropped mid-flight | `idleTimeout` reached in both directions |
| Traffic loops or hangs | An upstream target points back at this relay's own public address |
