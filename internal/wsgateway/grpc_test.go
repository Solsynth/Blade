package wsgateway

import (
	"context"
	"testing"

	gen "git.solsynth.dev/sosys/spec/gen/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGRPCService_GetWebsocketConnectionStatus(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "d1",
	}

	server := NewGRPCService(svc)

	byUser, err := server.GetWebsocketConnectionStatus(context.Background(), &gen.DyGetWebsocketConnectionStatusRequest{
		Id: &gen.DyGetWebsocketConnectionStatusRequest_UserId{UserId: "u1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !byUser.GetIsConnected() {
		t.Fatal("expected user to be connected")
	}

	byDevice, err := server.GetWebsocketConnectionStatus(context.Background(), &gen.DyGetWebsocketConnectionStatusRequest{
		Id: &gen.DyGetWebsocketConnectionStatusRequest_DeviceId{DeviceId: "d1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !byDevice.GetIsConnected() {
		t.Fatal("expected device to be connected")
	}
}

func TestGRPCService_GetWebsocketConnectionStatusBatch(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "d1",
	}

	server := NewGRPCService(svc)
	resp, err := server.GetWebsocketConnectionStatusBatch(context.Background(), &gen.DyGetWebsocketConnectionStatusBatchRequest{
		UsersId: []string{"u1", "u2"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.GetIsConnected()["u1"] {
		t.Fatal("expected u1 connected=true")
	}
	if resp.GetIsConnected()["u2"] {
		t.Fatal("expected u2 connected=false")
	}
}

func TestGRPCService_GetAllConnectedUserIds(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	svc.connections[connectionKey{accountID: "u2", deviceID: "d2"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u2"},
		deviceID: "d2",
	}
	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  &gen.DyAccount{Id: "u1"},
		deviceID: "d1",
	}

	server := NewGRPCService(svc)
	resp, err := server.GetAllConnectedUserIds(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetUserIds()) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.GetUserIds()))
	}
	if resp.GetUserIds()[0] != "u1" || resp.GetUserIds()[1] != "u2" {
		t.Fatalf("expected sorted users [u1 u2], got %#v", resp.GetUserIds())
	}
}

func TestGRPCService_ReceiveWebSocketPacket_Validation(t *testing.T) {
	server := NewGRPCService(NewService(Config{}, nil, nil, nil))

	_, err := server.ReceiveWebSocketPacket(context.Background(), &gen.DyReceiveWebSocketPacketRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument, got %v", status.Code(err))
	}
}
