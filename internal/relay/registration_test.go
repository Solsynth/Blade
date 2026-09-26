package relay

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"
)

// fakeDiscovery is an in-process DyServiceDiscoveryService that records what
// the relay publishes.
type fakeDiscovery struct {
	gen.UnimplementedDyServiceDiscoveryServiceServer

	mu          sync.Mutex
	registers   int
	renews      int
	deregisters int
	lastAuth    string
	lastRequest *gen.DyRegisterServiceInstanceRequest
	lease       time.Duration
}

func (f *fakeDiscovery) Register(ctx context.Context, req *gen.DyRegisterServiceInstanceRequest) (*gen.DyRegisterServiceInstanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers++
	f.lastAuth = authHeader(ctx)
	f.lastRequest = req
	return &gen.DyRegisterServiceInstanceResponse{
		Instance:             req.GetInstance(),
		LeaseExpiresAtUnixMs: time.Now().Add(f.lease).UnixMilli(),
	}, nil
}

func (f *fakeDiscovery) Renew(ctx context.Context, req *gen.DyRenewServiceLeaseRequest) (*gen.DyRenewServiceLeaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	f.lastAuth = authHeader(ctx)
	return &gen.DyRenewServiceLeaseResponse{
		LeaseExpiresAtUnixMs: time.Now().Add(f.lease).UnixMilli(),
	}, nil
}

func (f *fakeDiscovery) Deregister(ctx context.Context, req *gen.DyDeregisterServiceInstanceRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregisters++
	f.lastAuth = authHeader(ctx)
	return &emptypb.Empty{}, nil
}

func (f *fakeDiscovery) snapshot() (registers, renews, deregisters int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers, f.renews, f.deregisters
}

func (f *fakeDiscovery) registerRequest() *gen.DyRegisterServiceInstanceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequest
}

func (f *fakeDiscovery) authorization() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

func authHeader(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("authorization"); len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func newDiscoveryClient(t *testing.T, fake *fakeDiscovery) gen.DyServiceDiscoveryServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	gen.RegisterDyServiceDiscoveryServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gen.NewDyServiceDiscoveryServiceClient(conn)
}

func discoveryConfig() Config {
	cfg := validConfig()
	cfg.Discovery.Enabled = true
	cfg.Discovery.Service = "relay"
	cfg.Discovery.RegistrationToken = "smoke-token"
	cfg.Discovery.LeaseSeconds = 30
	cfg.Relay.Weight = 2
	cfg.Relay.Region = "jp"
	cfg.Relay.PublicHost = "relay-jp.solian.app"
	cfg.Relay.PublicPort = 7443
	cfg.Health.Advertise = "http://10.0.0.5:8081"
	return *cfg
}

func TestRegistrationPublishesRelayInstance(t *testing.T) {
	fake := &fakeDiscovery{lease: time.Hour}
	registration := NewRegistration(newDiscoveryClient(t, fake), discoveryConfig())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registration.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, func() bool {
		registers, _, _ := fake.snapshot()
		return registers == 1
	}, "the relay registration")

	request := fake.registerRequest()
	instance := request.GetInstance()
	if instance.GetService() != "relay" || instance.GetInstanceId() != "jp-01" {
		t.Fatalf("instance identity = %s/%s, want relay/jp-01", instance.GetService(), instance.GetInstanceId())
	}
	if got := instance.GetEndpoints()["tcp"]; got != "relay-jp.solian.app:7443" {
		t.Fatalf("tcp endpoint = %q, want the public address", got)
	}
	if got := instance.GetEndpoints()["http"]; got != "http://10.0.0.5:8081" {
		t.Fatalf("http endpoint = %q, want the advertised health address", got)
	}
	if instance.GetMetadata()["region"] != "jp" || instance.GetWeight() != 2 {
		t.Fatalf("metadata/weight = %v/%d, want region jp and weight 2", instance.GetMetadata(), instance.GetWeight())
	}
	if request.GetLeaseSeconds() != 30 {
		t.Fatalf("lease_seconds = %d, want 30", request.GetLeaseSeconds())
	}
	if fake.authorization() != "Bearer smoke-token" {
		t.Fatalf("authorization = %q, want the bearer token", fake.authorization())
	}
}

func TestRegistrationRenewsAndDeregisters(t *testing.T) {
	fake := &fakeDiscovery{lease: time.Second}
	cfg := discoveryConfig()
	cfg.Discovery.LeaseSeconds = 3
	registration := NewRegistration(newDiscoveryClient(t, fake), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registration.Run(ctx)
		close(done)
	}()

	// A one second lease renews roughly every 333ms.
	waitFor(t, func() bool {
		_, renews, _ := fake.snapshot()
		return renews >= 1
	}, "a lease renewal")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	registration.Deregister(context.Background())
	if _, _, deregisters := fake.snapshot(); deregisters != 1 {
		t.Fatalf("deregisters = %d, want 1", deregisters)
	}
}

func TestRegistrationRegistersBeforeTheLeaseExpires(t *testing.T) {
	fake := &fakeDiscovery{lease: time.Hour}
	registration := NewRegistration(newDiscoveryClient(t, fake), discoveryConfig())

	// Deregister on a never-registered node is a no-op, not a call.
	registration.Deregister(context.Background())
	if _, _, deregisters := fake.snapshot(); deregisters != 0 {
		t.Fatalf("deregisters = %d, want no call before a successful registration", deregisters)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registration.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool {
		registers, _, _ := fake.snapshot()
		return registers == 1
	}, "the relay registration")
	cancel()
	<-done
}

func TestRenewalInterval(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name            string
		expiresAtUnixMs int64
		leaseSeconds    int32
		wantMin         time.Duration
		wantMax         time.Duration
	}{
		{"third of the remaining lease", now.Add(9 * time.Second).UnixMilli(), 30, 2900 * time.Millisecond, 3100 * time.Millisecond},
		{"unknown expiry falls back to the lease", 0, 30, 9900 * time.Millisecond, 10100 * time.Millisecond},
		{"expired lease falls back to the lease", now.Add(-time.Minute).UnixMilli(), 30, 9900 * time.Millisecond, 10100 * time.Millisecond},
		{"short lease clamps to one second", 0, 2, time.Second, time.Second},
		{"short remaining lease clamps to one second", now.Add(2 * time.Second).UnixMilli(), 30, time.Second, time.Second},
	}

	for _, tc := range cases {
		got := RenewalInterval(tc.expiresAtUnixMs, tc.leaseSeconds, now)
		if got < tc.wantMin || got > tc.wantMax {
			t.Errorf("%s: RenewalInterval() = %v, want in [%v, %v]", tc.name, got, tc.wantMin, tc.wantMax)
		}
	}
}
