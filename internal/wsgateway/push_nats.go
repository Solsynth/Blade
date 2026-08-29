package wsgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/encoding/protojson"
	gen "src.solsynth.dev/sosys/go/proto"
)

const pushSubjectSuffix = "push"

type PushPublisher interface {
	PublishAccount(ctx context.Context, namespace, accountID string, packet *gen.DyWebSocketPacket, excluded []string) error
	PublishDevices(ctx context.Context, namespace string, deviceIDs []string, packet *gen.DyWebSocketPacket) error
}

type natsPushEvent struct {
	Target            string   `json:"target"`
	Namespace         string   `json:"namespace"`
	IDs               []string `json:"ids"`
	ExcludedDeviceIDs []string `json:"excluded_device_ids,omitempty"`
	Packet            []byte   `json:"packet"`
}

type NATSPushPublisher struct {
	conn    *nats.Conn
	subject string
}

func NewNATSPushPublisher(conn *nats.Conn, subjectPrefix string) *NATSPushPublisher {
	return &NATSPushPublisher{conn: conn, subject: strings.TrimSpace(subjectPrefix) + pushSubjectSuffix}
}

func (p *NATSPushPublisher) PublishAccount(_ context.Context, namespace, accountID string, packet *gen.DyWebSocketPacket, excluded []string) error {
	return p.publish(natsPushEvent{Target: "account", Namespace: namespace, IDs: []string{accountID}, ExcludedDeviceIDs: uniqueTrimmedStrings(excluded)}, packet)
}

func (p *NATSPushPublisher) PublishDevices(_ context.Context, namespace string, deviceIDs []string, packet *gen.DyWebSocketPacket) error {
	return p.publish(natsPushEvent{Target: "device", Namespace: namespace, IDs: uniqueTrimmedStrings(deviceIDs)}, packet)
}

func (p *NATSPushPublisher) publish(event natsPushEvent, packet *gen.DyWebSocketPacket) error {
	if p == nil || p.conn == nil || packet == nil {
		return fmt.Errorf("nats push publisher and packet are required")
	}
	packetBytes, err := protojson.Marshal(packet)
	if err != nil {
		return fmt.Errorf("marshal websocket push packet: %w", err)
	}
	event.Packet = packetBytes
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal websocket push event: %w", err)
	}
	return p.conn.Publish(p.subject, body)
}

func SubscribeWebSocketPushes(conn *nats.Conn, subjectPrefix string, svc *Service) (*nats.Subscription, error) {
	if conn == nil || svc == nil {
		return nil, fmt.Errorf("nats connection and websocket service are required")
	}
	subject := strings.TrimSpace(subjectPrefix) + pushSubjectSuffix
	return conn.Subscribe(subject, func(msg *nats.Msg) {
		var event natsPushEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}
		var packet gen.DyWebSocketPacket
		if err := protojson.Unmarshal(event.Packet, &packet); err != nil {
			return
		}
		namespace := event.Namespace
		if namespace == "" {
			namespace = svc.GetDefaultNamespace()
		}
		switch event.Target {
		case "account":
			svc.SendPacketToAccountsExcept(namespace, event.IDs, &packet, event.ExcludedDeviceIDs)
		case "device":
			for _, deviceID := range uniqueTrimmedStrings(event.IDs) {
				svc.SendPacketToDevice(namespace, deviceID, &packet)
			}
		}
	})
}
