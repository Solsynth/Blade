package health

import (
	"encoding/json"
	"testing"
	"time"
)

func TestServiceStateUsesSnakeCaseJSON(t *testing.T) {
	payload, err := json.Marshal(ServiceState{
		ServiceName: "sphere",
		IsHealthy:   true,
		LastChecked: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var state map[string]json.RawMessage
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := state["service_name"]; !ok {
		t.Fatalf("expected service_name in %s", payload)
	}
	if _, ok := state["ServiceName"]; ok {
		t.Fatalf("unexpected Pascal-case service name in %s", payload)
	}
}
