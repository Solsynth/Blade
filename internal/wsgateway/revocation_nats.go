package wsgateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"srv.solsynth.dev/sosys/blade/internal/logging"
	"github.com/nats-io/nats.go"
)

const AuthSessionRevokedSubject = "auth.session.revoked"

type authSessionRevokedEvent struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	SessionID string `json:"session_id"`
	AccountID string `json:"account_id"`
	ClientID  string `json:"client_id"`
	DeviceID  string `json:"device_id"`
}

func SubscribeAuthSessionRevocations(conn *nats.Conn, svc *Service) (*nats.Subscription, error) {
	if conn == nil {
		return nil, fmt.Errorf("nats connection is required")
	}
	if svc == nil {
		return nil, fmt.Errorf("websocket service is required")
	}

	return conn.Subscribe(AuthSessionRevokedSubject, func(msg *nats.Msg) {
		handleAuthSessionRevokedMessage(svc, msg)
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func handleAuthSessionRevokedMessage(svc *Service, msg *nats.Msg) {
	if msg == nil || svc == nil {
		return
	}

	var event authSessionRevokedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logging.Log.Warn().Err(err).Str("subject", msg.Subject).Msg("Failed to decode auth session revoked event")
		return
	}

	disconnected := svc.DisconnectSession(event.SessionID, event.AccountID, firstNonEmpty(event.DeviceID, event.ClientID), "session logged out; please reconnect")
	if disconnected == 0 {
		logging.Log.Debug().
			Str("subject", msg.Subject).
			Str("sessionId", strings.TrimSpace(event.SessionID)).
			Str("accountId", strings.TrimSpace(event.AccountID)).
			Str("deviceId", strings.TrimSpace(firstNonEmpty(event.DeviceID, event.ClientID))).
			Msg("Ignored auth session revoked event because no websocket connection matched")
		return
	}

	logging.Log.Info().
		Str("subject", msg.Subject).
		Str("eventId", strings.TrimSpace(event.EventID)).
		Str("eventType", strings.TrimSpace(event.EventType)).
		Str("sessionId", strings.TrimSpace(event.SessionID)).
		Str("accountId", strings.TrimSpace(event.AccountID)).
		Str("deviceId", strings.TrimSpace(firstNonEmpty(event.DeviceID, event.ClientID))).
		Int("disconnectCount", disconnected).
		Msg("Disconnected websocket connections after auth session revocation")
}
