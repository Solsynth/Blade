package wsgateway

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	gen "src.solsynth.dev/sosys/go/proto"
)

type recordingPushPublisher struct {
	namespace string
	accountID string
	deviceIDs []string
	packet    *gen.DyWebSocketPacket
	excluded  []string
}

func (p *recordingPushPublisher) PublishAccount(_ context.Context, namespace, accountID string, packet *gen.DyWebSocketPacket, excluded []string) error {
	p.namespace, p.accountID, p.packet, p.excluded = namespace, accountID, packet, excluded
	return nil
}

func (p *recordingPushPublisher) PublishDevices(_ context.Context, namespace string, deviceIDs []string, packet *gen.DyWebSocketPacket) error {
	p.namespace, p.deviceIDs, p.packet = namespace, deviceIDs, packet
	return nil
}

func TestGRPCService_PushWebSocketPacketPublishesWhenConfigured(t *testing.T) {
	publisher := &recordingPushPublisher{}
	server := NewGRPCService(NewService(Config{}, nil, nil, nil, nil, nil, nil))
	server.SetPushPublisher(publisher)
	packet := &gen.DyWebSocketPacket{Type: "event"}

	_, err := server.PushWebSocketPacket(context.Background(), &gen.DyPushWebSocketPacketRequest{
		UserId:                     "u1",
		Packet:                     packet,
		ExcludedWebsocketDeviceIds: []string{" d1 ", "d1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if publisher.accountID != "u1" || publisher.packet != packet {
		t.Fatalf("unexpected publisher call: %#v", publisher)
	}
	if !reflect.DeepEqual(publisher.excluded, []string{"d1"}) {
		t.Fatalf("unexpected exclusions: %#v", publisher.excluded)
	}
}

func TestGRPCService_GetWebsocketConnectionStatus(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u1", deviceID: "d1"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u1"},
		deviceID:  "d1",
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
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u1", deviceID: "d1"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u1"},
		deviceID:  "d1",
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

func TestGRPCService_GetConnectedWebsocketDeviceIds(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u1", deviceID: "d1"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u1"},
		deviceID:  "d1",
	}

	server := NewGRPCService(svc)
	resp, err := server.GetConnectedWebsocketDeviceIds(context.Background(), &gen.DyGetConnectedWebsocketDeviceIdsRequest{
		DeviceIds: []string{"d2", "d1", "d1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(resp.GetDeviceIds(), []string{"d1"}) {
		t.Fatalf("expected connected device [d1], got %#v", resp.GetDeviceIds())
	}
}

func TestGRPCService_GetAllConnectedUserIds(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u2", deviceID: "d2"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u2"},
		deviceID:  "d2",
	}
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u1", deviceID: "d1"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u1"},
		deviceID:  "d1",
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
	server := NewGRPCService(NewService(Config{}, nil, nil, nil, nil, nil, nil))

	_, err := server.ReceiveWebSocketPacket(context.Background(), &gen.DyReceiveWebSocketPacketRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument, got %v", status.Code(err))
	}
}

func TestUniqueTrimmedStrings(t *testing.T) {
	got := uniqueTrimmedStrings([]string{" u1 ", "", "u2", "u1", " u2 ", "u3"})
	want := []string{"u1", "u2", "u3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %#v, got %#v", want, got)
	}
}
