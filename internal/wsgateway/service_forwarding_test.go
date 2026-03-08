package wsgateway

import (
	"context"
	"encoding/json"
	"testing"

	gen "git.solsynth.dev/sosys/spec/gen/go"
)

type testForwarder struct {
	called   bool
	account  *gen.DyAccount
	deviceID string
	endpoint string
	packet   *gen.DyWebSocketPacket
}

func (f *testForwarder) Forward(_ context.Context, account *gen.DyAccount, deviceID string, endpoint string, packet *gen.DyWebSocketPacket) error {
	f.called = true
	f.account = account
	f.deviceID = deviceID
	f.endpoint = endpoint
	f.packet = packet
	return nil
}

func TestServiceHandlePacket_ForwardsEndpointPacket(t *testing.T) {
	fwd := &testForwarder{}
	svc := NewService(Config{}, nil, fwd, nil)

	var pkt Packet
	raw := []byte(`{"type":"messages.test","endpoint":"messager","data":{"hello":"world"}}`)
	if err := json.Unmarshal(raw, &pkt); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	account := &gen.DyAccount{Id: "u1"}
	if err := svc.HandlePacket(context.Background(), account, "d1", pkt); err != nil {
		t.Fatalf("unexpected handle error: %v", err)
	}

	if !fwd.called {
		t.Fatal("expected forwarder to be called")
	}
	if fwd.endpoint != "messager" {
		t.Fatalf("expected endpoint messager, got %q", fwd.endpoint)
	}
	if fwd.packet == nil || string(fwd.packet.GetData()) != `{"hello":"world"}` {
		t.Fatalf("unexpected forwarded data: %s", string(fwd.packet.GetData()))
	}
}

