package wsgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.solsynth.dev/solarnetwork/blade/internal/logging"
	gen "git.solsynth.dev/solarnetwork/dysonproto/gen/go"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type NATSForwarderConfig struct {
	SubjectPrefix string
}

type natsWebSocketPacketEvent struct {
	EventID     string `json:"event_id"`
	Timestamp   string `json:"timestamp"`
	StreamName  string `json:"stream_name"`
	AccountID   string `json:"account_id"`
	DeviceID    string `json:"device_id"`
	PacketBytes []byte `json:"packet_bytes"`
}

type NatsForwarder struct {
	conn          *nats.Conn
	subjectPrefix string
}

func NewNatsForwarder(conn *nats.Conn, cfg NATSForwarderConfig) *NatsForwarder {
	prefix := strings.TrimSpace(cfg.SubjectPrefix)
	if prefix == "" {
		prefix = "websocket_"
	}
	return &NatsForwarder{
		conn:          conn,
		subjectPrefix: prefix,
	}
}

func (f *NatsForwarder) Forward(_ context.Context, account *gen.DyAccount, deviceID string, endpoint string, packet *gen.DyWebSocketPacket) error {
	if account == nil || strings.TrimSpace(account.GetId()) == "" {
		return fmt.Errorf("account is required for endpoint forwarding")
	}
	if packet == nil {
		return fmt.Errorf("packet is required for endpoint forwarding")
	}

	normalizedEndpoint := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(endpoint), "DysonNetwork.", ""))
	if normalizedEndpoint == "" {
		return fmt.Errorf("endpoint is required for endpoint forwarding")
	}
	subject := f.subjectPrefix + normalizedEndpoint

	wirePacket := packetFromProto(packet)
	wirePacket.Endpoint = endpoint

	packetBytes, err := json.Marshal(wirePacket)
	if err != nil {
		return fmt.Errorf("marshal packet json: %w", err)
	}

	eventBytes, err := json.Marshal(natsWebSocketPacketEvent{
		EventID:     uuid.NewString(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339Nano),
		StreamName:  "websocket_events",
		AccountID:   account.GetId(),
		DeviceID:    deviceID,
		PacketBytes: packetBytes,
	})
	if err != nil {
		return fmt.Errorf("marshal websocket event: %w", err)
	}

	if err := f.conn.Publish(subject, eventBytes); err != nil {
		return fmt.Errorf("publish to nats subject %s: %w", subject, err)
	}

	logging.Log.Debug().Str("subject", subject).Str("endpoint", endpoint).Str("account", account.Name).Msg("Forwarded websocket packet")

	return nil
}
