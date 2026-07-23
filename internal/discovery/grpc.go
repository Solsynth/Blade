package discovery

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

// GRPCService exposes the shared discovery contract. Mutating calls require a
// service credential; resolution is intentionally read-only.
type GRPCService struct {
	gen.UnimplementedDyServiceDiscoveryServiceServer
	registry          *Registry
	registrationToken string
}

func NewGRPCService(registry *Registry, registrationToken string) *GRPCService {
	return &GRPCService{registry: registry, registrationToken: strings.TrimSpace(registrationToken)}
}

func (s *GRPCService) Register(ctx context.Context, req *gen.DyRegisterServiceInstanceRequest) (*gen.DyRegisterServiceInstanceResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	instance, expiresAt, err := s.registry.Register(ctx, req.GetInstance(), leaseFromSeconds(req.GetLeaseSeconds(), s.registry.DefaultLease()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	logging.Log.Info().
		Str("service", instance.GetService()).
		Str("instance", instance.GetInstanceId()).
		Str("http_endpoint", Endpoint(instance, "http")).
		Str("grpc_endpoint", Endpoint(instance, "grpc")).
		Time("lease_expires_at", expiresAt).
		Msg("Service instance registered")
	return &gen.DyRegisterServiceInstanceResponse{Instance: instance, LeaseExpiresAtUnixMs: expiresAt.UnixMilli()}, nil
}

func (s *GRPCService) Renew(ctx context.Context, req *gen.DyRenewServiceLeaseRequest) (*gen.DyRenewServiceLeaseResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	expiresAt, err := s.registry.Renew(ctx, req.GetService(), req.GetInstanceId(), leaseFromSeconds(req.GetLeaseSeconds(), s.registry.DefaultLease()))
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &gen.DyRenewServiceLeaseResponse{LeaseExpiresAtUnixMs: expiresAt.UnixMilli()}, nil
}

func (s *GRPCService) Deregister(ctx context.Context, req *gen.DyDeregisterServiceInstanceRequest) (*emptypb.Empty, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.registry.Deregister(ctx, req.GetService(), req.GetInstanceId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) Resolve(ctx context.Context, req *gen.DyResolveServiceRequest) (*gen.DyResolveServiceResponse, error) {
	if req == nil || strings.TrimSpace(req.GetService()) == "" {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}
	instances, err := s.registry.List(ctx, req.GetService())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if req.GetHealthyOnly() {
		filtered := instances[:0]
		for _, instance := range instances {
			if instance.GetHealthy() {
				filtered = append(filtered, instance)
			}
		}
		instances = filtered
	}
	return &gen.DyResolveServiceResponse{Instances: instances}, nil
}

func (s *GRPCService) authorize(ctx context.Context) error {
	if s.registrationToken == "" {
		return status.Error(codes.FailedPrecondition, "service discovery registration is not configured")
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing authorization")
	}
	for _, value := range md.Get("authorization") {
		if strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")) == s.registrationToken {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid service credential")
}

func leaseFromSeconds(seconds int32, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
