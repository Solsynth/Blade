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

### `[health]`

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `":8081"` | Status listener; must be reachable by the discovery master |
| `advertise` | `http://<publicHost>:<healthPort>` | Endpoint published for probing |

Set `advertise` explicitly to an internal address whenever the public hostname
is not resolvable from the discovery master.

### `[discovery]`

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Register this node; when false the node serves traffic but is never listed |
| `target` | `""` | Discovery gRPC endpoint, e.g. `blade:7001` |
| `useTLS` | `false` | TLS to the discovery endpoint |
| `tlsSkipVerify` | `false` | Skip verification (self-signed fleets only) |
| `tlsServerName` | `""` | Override SNI for the discovery channel |
| `registrationToken` | `""` | Required; sent as `authorization: Bearer <token>` |
| `service` | `"relay"` | Registry service name; the master's catalog key must match |
| `leaseSeconds` | `30` | Requested lease (≥3) |

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
- `health.listen` must be reachable from the discovery master and should not be
  exposed publicly: it dials upstreams on request, so treat it as internal.
- `discovery.target` must be reachable from the relay.

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
  -p 443:443 -p 8081:8081 \
  -v /etc/blade/relay.toml:/app/configs/relay.toml:ro \
  -e RELAY_CONFIG_PATH=/app/configs/relay.toml \
  blade-relay
```

Publish only `relay.listen` externally. In containers `relay.id` must be set
explicitly: the default hostname is the container id, so replicas would
overwrite each other in the registry.

### Fleet notes

- One registration per `service` + `id`. Two relays on one host need distinct
  `id` values.
- `region` and `weight` are what clients use to pick between nodes, so fill
  them in per PoP.
- Discovery registration starts unhealthy; the master flips it healthy after its
  probe of `health.advertise/health` succeeds, so a fresh node appears in the
  catalog a probe cycle later.
- Register failures retry with exponential backoff (5s doubling to a 30s cap);
  a lost lease re-registers instead of renewing.

## Verification

```bash
# The relay's own view: upstreams reachable, live counters
curl -s localhost:8081/health     # 200 "ok", or 503 {"status":"unhealthy","failures":{...}}
curl -s localhost:8081/status

# End-to-end TLS through the node (certificate must still be the origin's)
openssl s_client -connect <publicHost>:443 -servername <allowlisted sni> </dev/null | head -20
openssl s_client -connect <publicHost>:443 -servername unlisted.example </dev/null 2>&1 | tail -3

# Listed by the master
curl -s https://<api-host>/relays
```

A reject-by-default node aborts the unlisted-SNI handshake with no route
presented. `/status` reports `accepted`, `rejected` (over the connection cap),
`sniRejects` (no usable ClientHello or no upstream for the SNI), `dialErrors`,
`bytesUp`, `bytesDown` (relayed bytes, excluding the peeked ClientHello), and
`uptimeSeconds`.

## Operations

`SIGTERM` stops accepting, closes live relayed connections, withdraws the
registration, and stops the status listener, inside a 5 second budget. Live
connections are cut rather than drained, so drains should be done by shifting
client traffic first. The lease expires on its own if the withdraw call fails.

Capacity: each relayed connection costs two goroutines and roughly 32 KiB of
copy buffer per direction, on top of the socket buffers. Size
`maxConnections` from those figures, not from bandwidth.

Common failures:

| Symptom | Cause |
| --- | --- |
| Process exits with a config error | Missing upstream rule, target without a port, duplicate SNI, or discovery enabled without token/`publicHost` |
| `/health` returns 503 with `failures` | The node cannot reach its own upstreams; fix origin reachability |
| Node never appears in the catalog | Discovery disabled, wrong `service` name, rejected `registrationToken`, or `health.advertise` unreachable |
| Catalog entry stays `healthy: false` | The master's probe of `health.advertise/health` is failing |
| Handshakes fail for one name only | That SNI has no rule and no `defaultUpstream` is set, or the client sends no cleartext SNI (ECH) |
| Connections reset under load | `maxConnections` reached; watch `rejected` on `/status` |
| Connections dropped mid-flight | `idleTimeout` reached in both directions |
| Traffic loops or hangs | An upstream target points back at this relay's own public address |
