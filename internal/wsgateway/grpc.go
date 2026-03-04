package wsgateway

import (
	"context"
	"strings"

	gen "git.solsynth.dev/solarnetwork/dysonproto/gen/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type GRPCService struct {
	gen.UnimplementedWebSocketServiceServer
	service *Service
}

func NewGRPCService(service *Service) *GRPCService {
	return &GRPCService{service: service}
}

func (s *GRPCService) PushWebSocketPacket(_ context.Context, req *gen.DyPushWebSocketPacketRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetUserId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	s.service.SendPacketToAccount(req.GetUserId(), req.GetPacket())
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToUsers(_ context.Context, req *gen.DyPushWebSocketPacketToUsersRequest) (*emptypb.Empty, error) {
	if req == nil || len(req.GetUserIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "user_ids is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	for _, userID := range req.GetUserIds() {
		if strings.TrimSpace(userID) == "" {
			continue
		}
		s.service.SendPacketToAccount(userID, req.GetPacket())
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToDevice(_ context.Context, req *gen.DyPushWebSocketPacketToDeviceRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetDeviceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	s.service.SendPacketToDevice(req.GetDeviceId(), req.GetPacket())
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) PushWebSocketPacketToDevices(_ context.Context, req *gen.DyPushWebSocketPacketToDevicesRequest) (*emptypb.Empty, error) {
	if req == nil || len(req.GetDeviceIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "device_ids is required")
	}
	if req.GetPacket() == nil {
		return nil, status.Error(codes.InvalidArgument, "packet is required")
	}
	for _, deviceID := range req.GetDeviceIds() {
		if strings.TrimSpace(deviceID) == "" {
			continue
		}
		s.service.SendPacketToDevice(deviceID, req.GetPacket())
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCService) GetWebsocketConnectionStatus(_ context.Context, req *gen.DyGetWebsocketConnectionStatusRequest) (*gen.DyGetWebsocketConnectionStatusResponse, error) {
	if req == nil || req.GetId() == nil {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	var connected bool
	switch id := req.GetId().(type) {
	case *gen.DyGetWebsocketConnectionStatusRequest_DeviceId:
		if strings.TrimSpace(id.DeviceId) == "" {
			return nil, status.Error(codes.InvalidArgument, "device_id is required")
		}
		connected = s.service.GetDeviceIsConnected(id.DeviceId)
	case *gen.DyGetWebsocketConnectionStatusRequest_UserId:
		if strings.TrimSpace(id.UserId) == "" {
			return nil, status.Error(codes.InvalidArgument, "user_id is required")
		}
		connected = s.service.GetAccountIsConnected(id.UserId)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported id type")
	}

	return &gen.DyGetWebsocketConnectionStatusResponse{IsConnected: connected}, nil
}

func (s *GRPCService) GetWebsocketConnectionStatusBatch(_ context.Context, req *gen.DyGetWebsocketConnectionStatusBatchRequest) (*gen.DyGetWebsocketConnectionStatusBatchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	result := make(map[string]bool, len(req.GetUsersId()))
	for _, userID := range req.GetUsersId() {
		if strings.TrimSpace(userID) == "" {
			continue
		}
		result[userID] = s.service.GetAccountIsConnected(userID)
	}

	return &gen.DyGetWebsocketConnectionStatusBatchResponse{IsConnected: result}, nil
}

func (s *GRPCService) GetAllConnectedUserIds(_ context.Context, _ *emptypb.Empty) (*gen.DyGetAllConnectedUserIdsResponse, error) {
	return &gen.DyGetAllConnectedUserIdsResponse{
		UserIds: s.service.GetAllConnectedUserIDs(),
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

	packet := packetFromProto(req.GetPacket())
	if err := s.service.HandlePacket(ctx, req.GetAccount(), req.GetDeviceId(), packet); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to handle packet: %v", err)
	}

	return &emptypb.Empty{}, nil
}
