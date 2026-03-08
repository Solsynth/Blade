package wsgateway

import (
	"testing"

	"github.com/google/uuid"
)

func TestServiceNormalizeDeviceID_UsesProvidedValue(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)

	got := svc.normalizeDeviceID("  device-123  ")
	if got != "device-123" {
		t.Fatalf("expected trimmed device id, got %q", got)
	}
}

func TestServiceNormalizeDeviceID_GeneratesUUIDWhenMissing(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)

	got := svc.normalizeDeviceID("   ")
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("expected generated UUID, got %q: %v", got, err)
	}
}

