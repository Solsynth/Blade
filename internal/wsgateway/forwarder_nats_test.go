package wsgateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type recordedNATSPublish struct {
	subject string
	data    []byte
}

type recordingNATSPublisher struct {
	publishes []recordedNATSPublish
}

func (p *recordingNATSPublisher) Publish(subject string, data []byte) error {
	p.publishes = append(p.publishes, recordedNATSPublish{subject: subject, data: data})
	return nil
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strip canonical prefix",
			in:   "DysonNetwork.Messager",
			want: "messager",
		},
		{
			name: "strip lowercase prefix",
			in:   "dysonnetwork.Messager",
			want: "messager",
		},
		{
			name: "trim whitespace",
			in:   "  DysonNetwork.Messager.Events  ",
			want: "messager.events",
		},
		{
			name: "no prefix keeps endpoint",
			in:   "Messager",
			want: "messager",
		},
		{
			name: "empty remains empty",
			in:   "   ",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEndpoint(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNatsForwarderPublishConnected(t *testing.T) {
	publisher := &recordingNATSPublisher{}
	forwarder := &NatsForwarder{conn: publisher, subjectPrefix: "websocket_"}

	if err := forwarder.PublishConnected(context.Background(), "u1", "d1"); err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}

	if len(publisher.publishes) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(publisher.publishes))
	}
	if publisher.publishes[0].subject != "websocket_connected" {
		t.Fatalf("expected subject websocket_connected, got %q", publisher.publishes[0].subject)
	}

	var event natsWebSocketConnectionEvent
	if err := json.Unmarshal(publisher.publishes[0].data, &event); err != nil {
		t.Fatalf("failed to unmarshal event: %v", err)
	}
	assertConnectionEvent(t, event, "connected", "u1", "d1", false)
}

func TestNatsForwarderPublishDisconnected(t *testing.T) {
	publisher := &recordingNATSPublisher{}
	forwarder := &NatsForwarder{conn: publisher, subjectPrefix: "websocket_"}

	if err := forwarder.PublishDisconnected(context.Background(), "u1", "d1", true); err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}

	if len(publisher.publishes) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(publisher.publishes))
	}
	if publisher.publishes[0].subject != "websocket_disconnected" {
		t.Fatalf("expected subject websocket_disconnected, got %q", publisher.publishes[0].subject)
	}

	var event natsWebSocketConnectionEvent
	if err := json.Unmarshal(publisher.publishes[0].data, &event); err != nil {
		t.Fatalf("failed to unmarshal event: %v", err)
	}
	assertConnectionEvent(t, event, "disconnected", "u1", "d1", true)
}

func assertConnectionEvent(t *testing.T, event natsWebSocketConnectionEvent, eventType string, accountID string, deviceID string, isOffline bool) {
	t.Helper()

	if event.EventID == "" {
		t.Fatal("expected event id")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
		t.Fatalf("expected RFC3339Nano timestamp, got %q: %v", event.Timestamp, err)
	}
	if event.StreamName != "websocket_connections" {
		t.Fatalf("expected stream websocket_connections, got %q", event.StreamName)
	}
	if event.EventType != eventType {
		t.Fatalf("expected event type %q, got %q", eventType, event.EventType)
	}
	if event.AccountID != accountID {
		t.Fatalf("expected account id %q, got %q", accountID, event.AccountID)
	}
	if event.DeviceID != deviceID {
		t.Fatalf("expected device id %q, got %q", deviceID, event.DeviceID)
	}
	if event.IsOffline != isOffline {
		t.Fatalf("expected isOffline %v, got %v", isOffline, event.IsOffline)
	}
}
