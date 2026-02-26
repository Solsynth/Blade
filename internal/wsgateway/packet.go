package wsgateway

import (
	"encoding/json"

	gen "git.solsynth.dev/solarnetwork/dysonproto/gen/go"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	PacketTypePing  = "ping"
	PacketTypePong  = "pong"
	PacketTypeError = "error"
)

// Packet is the on-wire websocket packet structure (JSON over binary frame).
type Packet struct {
	Type         string          `json:"type"`
	Data         json.RawMessage `json:"data,omitempty"`
	Endpoint     string          `json:"endpoint,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
}

func packetFromProto(p *gen.DyWebSocketPacket) Packet {
	if p == nil {
		return Packet{}
	}

	pkt := Packet{
		Type: p.GetType(),
		Data: p.GetData(),
	}

	if p.GetErrorMessage() != nil {
		pkt.ErrorMessage = p.GetErrorMessage().GetValue()
	}

	return pkt
}

func packetToProto(p Packet) *gen.DyWebSocketPacket {
	pkt := &gen.DyWebSocketPacket{
		Type: p.Type,
		Data: p.Data,
	}
	if p.ErrorMessage != "" {
		pkt.ErrorMessage = wrapperspb.String(p.ErrorMessage)
	}
	return pkt
}
