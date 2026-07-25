package wsgateway

import (
	"encoding/json"
	"testing"

	gen "src.solsynth.dev/sosys/go/proto"
	"github.com/nats-io/nats.go"
)

func TestHandleAuthSessionRevokedMessage_DisconnectsMatchingSession(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, nil, nil, nil)
	done := make(chan struct{})
	svc.connections[connectionKey{namespace: svc.cfg.DefaultNamespace, accountID: "u1", deviceID: "device-1"}] = &wsConnection{
		namespace: svc.cfg.DefaultNamespace,
		account:   &gen.DyAccount{Id: "u1"},
		sessionID: "session-1",
		deviceID:  "device-1",
		done:      done,
	}

	payload, err := json.Marshal(authSessionRevokedEvent{
		EventID:   "evt-1",
		EventType: AuthSessionRevokedSubject,
		SessionID: "session-1",
		AccountID: "u1",
		DeviceID:  "device-1",
	})
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}

	handleAuthSessionRevokedMessage(svc, &nats.Msg{
		Subject: AuthSessionRevokedSubject,
		Data:    payload,
	})

	select {
	case <-done:
	default:
		t.Fatal("expected websocket connection to be closed by revoked event")
	}
}
