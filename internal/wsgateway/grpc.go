package wsgateway

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"
)

type GRPCService struct {
	gen.UnimplementedWebSocketServiceServer
	service *Service
	pusher  PushPublisher
}

// SetPushPublisher keeps the gRPC API as an ingress path while distributing
// pushes to every gateway replica through the message bus.
func (s *GRPCService) SetPushPublisher(pusher PushPublisher) { s.pusher = pusher }

func NewGRPCService(service *Service) *GRPCService {
	return &GRPCService{service: service}
}

func (s *GRPCService) PushWebSocketPacket(ctx context.Context, req *gen.DyPushWebSocketPacketRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetUserId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	namespace := req.GetNamespace()
	if s.pusher != nil {
		if err := s.pusher.PublishAccount(ctx, namespace, req.GetUserId(), req.GetPacket(), uniqueTrimmedStrings(req.GetExcludedWebsocketDeviceIds())); err != nil {
			return nil, status.Errorf(codes.Unavailable, "publish websocket push: %v", err)
		}
	} else {
		s.service.SendPacketToAccountExcept(namespace, req.GetUserId(), req.GetPacket(), req.GetExcludedWebsocketDeviceIds())
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToUsers(ctx context.Context, req *gen.DyPushWebSocketPacketToUsersRequest) (*emptypb.Empty, error) {
	if req == nil || len(req.GetUserIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "user_ids is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	namespace := req.GetNamespace()
	for _, userID := range uniqueTrimmedStrings(req.GetUserIds()) {
		if s.pusher != nil {
			if err := s.pusher.PublishAccount(ctx, namespace, userID, req.GetPacket(), nil); err != nil {
				return nil, status.Errorf(codes.Unavailable, "publish websocket push: %v", err)
			}
		} else {
			s.service.SendPacketToAccount(namespace, userID, req.GetPacket())
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToDevice(ctx context.Context, req *gen.DyPushWebSocketPacketToDeviceRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetDeviceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	namespace := req.GetNamespace()
	if s.pusher != nil {
		if err := s.pusher.PublishDevices(ctx, namespace, []string{req.GetDeviceId()}, req.GetPacket()); err != nil {
			return nil, status.Errorf(codes.Unavailable, "publish websocket push: %v", err)
		}
	} else {
		s.service.SendPacketToDevice(namespace, req.GetDeviceId(), req.GetPacket())
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToDevices(ctx context.Context, req *gen.DyPushWebSocketPacketToDevicesRequest) (*emptypb.Empty, error) {
	if req == nil || len(req.GetDeviceIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "device_ids is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	deviceIDs := uniqueTrimmedStrings(req.GetDeviceIds())
	namespace := req.GetNamespace()
	if s.pusher != nil {
		if err := s.pusher.PublishDevices(ctx, namespace, deviceIDs, req.GetPacket()); err != nil {
			return nil, status.Errorf(codes.Unavailable, "publish websocket push: %v", err)
		}
	} else {
		for _, deviceID := range deviceIDs {
			s.service.SendPacketToDevice(namespace, deviceID, req.GetPacket())
		}
	}
	return &emptypb.Empty{}, nil
}

func uniqueTrimmedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func (s *GRPCService) GetWebsocketConnectionStatus(_ context.Context, req *gen.DyGetWebsocketConnectionStatusRequest) (*gen.DyGetWebsocketConnectionStatusResponse, error) {
	if req == nil || req.GetId() == nil {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	namespace := req.GetNamespace()

	var connected bool
	switch id := req.GetId().(type) {
	case *gen.DyGetWebsocketConnectionStatusRequest_DeviceId:
		if strings.TrimSpace(id.DeviceId) == "" {
			return nil, status.Error(codes.InvalidArgument, "device_id is required")
		}
		connected = s.service.GetDeviceIsConnected(namespace, id.DeviceId)
	case *gen.DyGetWebsocketConnectionStatusRequest_UserId:
		if strings.TrimSpace(id.UserId) == "" {
			return nil, status.Error(codes.InvalidArgument, "user_id is required")
		}
		connected = s.service.GetAccountIsConnected(namespace, id.UserId)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported id type")
	}

	return &gen.DyGetWebsocketConnectionStatusResponse{IsConnected: connected}, nil
}

func (s *GRPCService) GetWebsocketConnectionStatusBatch(_ context.Context, req *gen.DyGetWebsocketConnectionStatusBatchRequest) (*gen.DyGetWebsocketConnectionStatusBatchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	namespace := req.GetNamespace()

	result := make(map[string]bool, len(req.GetUsersId()))
	for _, userID := range req.GetUsersId() {
		if strings.TrimSpace(userID) == "" {
			continue
		}
		result[userID] = s.service.GetAccountIsConnected(namespace, userID)
	}

	return &gen.DyGetWebsocketConnectionStatusBatchResponse{IsConnected: result}, nil
}

func (s *GRPCService) GetConnectedWebsocketDeviceIds(_ context.Context, req *gen.DyGetConnectedWebsocketDeviceIdsRequest) (*gen.DyGetConnectedWebsocketDeviceIdsResponse, error) {
	if req == nil || len(req.GetDeviceIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "device_ids is required")
	}
	namespace := req.GetNamespace()

	return &gen.DyGetConnectedWebsocketDeviceIdsResponse{
		DeviceIds: s.service.GetConnectedDeviceIDs(namespace, req.GetDeviceIds()),
	}, nil
}

func (s *GRPCService) GetAllConnectedUserIds(_ context.Context, req *gen.DyGetAllConnectedUserIdsRequest) (*gen.DyGetAllConnectedUserIdsResponse, error) {
	namespace := ""
	if req != nil {
		namespace = req.GetNamespace()
	}
	return &gen.DyGetAllConnectedUserIdsResponse{
		UserIds: s.service.GetAllConnectedUserIDs(namespace),
	}, nil
}

func (s *GRPCService) ReceiveWebSocketPacket(ctx context.Context, req *gen.DyReceiveWebSocketPacketRequest) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	if req.GetAccount() == nil || strings.TrimSpace(req.GetAccount().GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "account is required")
	}
	if strings.TrimSpace(req.GetDeviceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	namespace := req.GetNamespace()

	packet := packetFromProto(req.GetPacket())
	if err := s.service.HandlePacket(ctx, req.GetAccount(), namespace, req.GetDeviceId(), packet); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to handle packet: %v", err)
	}

	return &emptypb.Empty{}, nil
}
