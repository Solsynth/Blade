package wsgateway

import (
	"testing"

	gen "git.solsynth.dev/sosys/spec/gen/go"
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

func TestServiceTryAddUniqueDevice_RejectsDuplicateDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  account1,
		deviceID: "d1",
		probe: func() bool {
			return true
		},
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d1", nil); ok {
		t.Fatal("expected duplicate device id to be rejected")
	}
}

func TestServiceTryAddUniqueDevice_AcceptsDifferentDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	if _, ok := svc.TryAddUniqueDevice(account1, "d1", nil); !ok {
		t.Fatal("expected first connection to be accepted")
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d2", nil); !ok {
		t.Fatal("expected different device id to be accepted")
	}
}

func TestServiceTryAddUniqueDevice_ReplacesStaleDuplicateDeviceID(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil)
	account1 := &gen.DyAccount{Id: "u1"}
	account2 := &gen.DyAccount{Id: "u2"}

	svc.connections[connectionKey{accountID: "u1", deviceID: "d1"}] = &wsConnection{
		account:  account1,
		deviceID: "d1",
		probe: func() bool {
			return false
		},
	}
	if _, ok := svc.TryAddUniqueDevice(account2, "d1", nil); !ok {
		t.Fatal("expected stale duplicate device id to be replaced")
	}

	accounts := svc.GetAccountsByDevice("d1")
	if len(accounts) != 1 || accounts[0] != "u2" {
		t.Fatalf("expected device d1 to belong only to u2 after replacement, got %#v", accounts)
	}
}
