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

func TestServiceNormalizeDeviceID_GeneratesUUIDWithDeviceAltSuffix(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)

	got := svc.normalizeDeviceID("+watch")
	const suffix = "+watch"
	if len(got) <= len(suffix) || got[len(got)-len(suffix):] != suffix {
		t.Fatalf("expected generated id to keep %q suffix, got %q", suffix, got)
	}

	base := got[:len(got)-len(suffix)]
	if _, err := uuid.Parse(base); err != nil {
		t.Fatalf("expected uuid base in generated id %q: %v", got, err)
	}
}
