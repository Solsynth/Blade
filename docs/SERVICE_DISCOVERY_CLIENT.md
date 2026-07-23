# Service Discovery Client Guide

Blade exposes `proto.DyServiceDiscoveryService` on its configured gRPC port.
Each running service instance registers itself, continuously renews a lease, and
removes itself during graceful shutdown. Blade uses the resulting healthy
instance set for HTTP gateway routing and makes it available to other services
through `Resolve`.

The contract is defined in `Spec/proto/discovery.proto` and generated in the
shared package `src.solsynth.dev/sosys/go/proto`.

## Prerequisites

Blade must be configured with discovery enabled, Redis, and a service
registration secret:

```toml
[cache]
redisUrl = "redis://redis:6379/0"

[discovery]
enabled = true
prefix = "blade:discovery"
leaseSeconds = 30
leaderLeaseSeconds = 15
registrationToken = "replace-with-a-long-random-secret"
```

Use an internal address for Blade's gRPC server. Do not expose registration on
the public internet. Production deployments should also use mTLS between
services and Blade.

## Instance identity and endpoints

Every replica needs a stable, unique `instance_id` for its lifetime. Good
values include a Kubernetes pod UID, Nomad allocation ID, or a process UUID
generated during startup. Do not use only the service name: replicas would
overwrite each other.

Register endpoints by protocol when the service provides them:

```text
service:     sphere
instance_id: sphere-7f4d8b9c-pod-1
endpoints:
  http: http://sphere-7f4d8b9c-pod-1:8000
  grpc: sphere-7f4d8b9c-pod-1:7005
  nats: nats://broker/sphere
weight: 1
```

The `http` endpoint must be an absolute URL. Blade actively probes
`<http endpoint>/health`; a newly registered instance is not routable until
that check succeeds. A gRPC-only instance can be returned by `Resolve` with
`healthy_only = false`, but it cannot currently become healthy through Blade's
HTTP health checker. Give it an HTTP health endpoint until gRPC health probing
is added.

Blade uses the `http` and `grpc` map entries for its own proxying and
capabilities requests. Other protocol keys are preserved and returned by
`Resolve` for consumers that understand them. Legacy `http_endpoint` and
`grpc_endpoint` fields remain supported, but corresponding map entries take
precedence.

## Registration lifecycle

1. Create the gRPC channel to Blade.
2. Add `authorization: Bearer <registrationToken>` metadata to all mutating
   calls.
3. Call `Register` after the service is ready to answer `/health`.
4. Renew well before expiry—typically every one-third of the granted lease.
5. If `Renew` returns `NotFound`, call `Register` again; the lease likely
   expired while Blade or Redis was unavailable.
6. On graceful shutdown, stop accepting work, call `Deregister`, then close
   the gRPC channel. Redis TTL remains the safety net if the process crashes.

`Register` always starts an instance unhealthy. Blade has one Redis-elected
health-check leader, so scaling Blade does not multiply active probes.

## Go client example

```go
package main

import (
    "context"
    "time"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/metadata"
    gen "src.solsynth.dev/sosys/go/proto"
)

func register(ctx context.Context, bladeAddr, token, instanceID string) (gen.DyServiceDiscoveryServiceClient, *grpc.ClientConn, context.Context, error) {
    conn, err := grpc.NewClient(bladeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        return nil, nil, nil, err
    }
    client := gen.NewDyServiceDiscoveryServiceClient(conn)
    authenticated := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
    _, err = client.Register(authenticated, &gen.DyRegisterServiceInstanceRequest{
        Instance: &gen.DyServiceInstance{
            Service: "sphere", InstanceId: instanceID,
            Endpoints: map[string]string{
                "http": "http://sphere:8000",
                "grpc": "sphere:7005",
            },
            Weight: 1,
        },
        LeaseSeconds: 30,
    })
    if err != nil {
        conn.Close()
        return nil, nil, nil, err
    }
    return client, conn, authenticated, nil
}

func renewLoop(ctx context.Context, client gen.DyServiceDiscoveryServiceClient, authenticated context.Context, instanceID string) {
    ticker := time.NewTicker(10 * time.Second) // one-third of a 30-second lease
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            _, _ = client.Deregister(authenticated, &gen.DyDeregisterServiceInstanceRequest{
                Service: "sphere", InstanceId: instanceID,
            })
            return
        case <-ticker.C:
            // Retry transient failures with bounded backoff. If this returns
            // NotFound, register the instance again before the next renewal.
            _, _ = client.Renew(authenticated, &gen.DyRenewServiceLeaseRequest{
                Service: "sphere", InstanceId: instanceID, LeaseSeconds: 30,
            })
        }
    }
}
```

Use TLS credentials rather than `insecure.NewCredentials()` outside trusted
local networks.

## Resolving another service

`Resolve` is read-only and does not require the registration secret. Ask for
healthy instances in normal request paths and choose an endpoint locally:

```go
response, err := client.Resolve(ctx, &gen.DyResolveServiceRequest{
    Service: "ring", HealthyOnly: true,
})
if err != nil || len(response.Instances) == 0 {
    // Return a retryable dependency-unavailable error.
}

// Choose one endpoint with round-robin, weighted round-robin, or a client-side
// load balancer, then maintain/reuse its gRPC connection.
endpoint := response.Instances[0].GrpcEndpoint
```

Cache resolved instances briefly (about one second) and retry a different
healthy instance on a connection failure. Do not cache forever: registrations,
health state, and leases can change at any time.

## Failure behavior

- A crashed or partitioned instance disappears after its Redis lease expires.
- A failed `/health` probe marks the instance unhealthy, removing it from
  Blade proxy routing and `Resolve(healthy_only = true)`.
- A Redis outage prevents new registration and renewal. Existing process-local
  callers should use bounded retries and surface dependency-unavailable errors
  rather than assuming a stale endpoint remains valid.
- Until a service has a registered instance, Blade continues to use its static
  `[services]` HTTP target. Once at least one discovered instance exists, only
  healthy discovered HTTP instances are selected for gateway routing.
