package wsgateway

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestPresenceStore connects to BLADE_TEST_REDIS_ADDR (default
// 127.0.0.1:6379) and skips when no Redis is reachable. Every store gets a
// unique key prefix that t.Cleanup deletes wholesale.
func newTestPresenceStore(t *testing.T, ttl time.Duration) *RedisPresenceStore {
	t.Helper()

	addr := os.Getenv("BLADE_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	opts, err := redis.ParseURL("redis://" + addr)
	if err != nil {
		t.Skipf("invalid BLADE_TEST_REDIS_ADDR %q: %v", addr, err)
	}
	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("redis unavailable at %s: %v", addr, err)
	}

	prefix := "ws:presence:test:" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_ = client.Del(ctx, keys...).Err()
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		_ = client.Close()
	})

	return NewRedisPresenceStore(client, prefix, ttl)
}

func TestRedisPresenceStoreAccountDeviceIDsCollapsesConnections(t *testing.T) {
	store := newTestPresenceStore(t, time.Minute)
	ctx := context.Background()
	const namespace = "test-ns"

	if err := store.Register(ctx, namespace, "acc-1", "device-1", "conn-1"); err != nil {
		t.Fatalf("register first connection: %v", err)
	}
	if err := store.Register(ctx, namespace, "acc-1", "device-1", "conn-2"); err != nil {
		t.Fatalf("register second connection: %v", err)
	}

	got, err := store.AccountDeviceIDs(ctx, namespace, "acc-1")
	if err != nil {
		t.Fatalf("read account devices: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"device-1"}) {
		t.Fatalf("expected a single device entry, got %#v", got)
	}
}

func TestRedisPresenceStoreAccountDeviceIDsAreNamespaceScopedAndSorted(t *testing.T) {
	store := newTestPresenceStore(t, time.Minute)
	ctx := context.Background()

	if err := store.Register(ctx, "ns-a", "acc-1", "device-b", "conn-b"); err != nil {
		t.Fatalf("register device-b: %v", err)
	}
	if err := store.Register(ctx, "ns-a", "acc-1", "device-a", "conn-a"); err != nil {
		t.Fatalf("register device-a: %v", err)
	}
	if err := store.Register(ctx, "ns-b", "acc-1", "device-other", "conn-other"); err != nil {
		t.Fatalf("register other-namespace device: %v", err)
	}

	got, err := store.AccountDeviceIDs(ctx, "ns-a", "acc-1")
	if err != nil {
		t.Fatalf("read ns-a devices: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"device-a", "device-b"}) {
		t.Fatalf("expected sorted ns-a devices, got %#v", got)
	}

	other, err := store.AccountDeviceIDs(ctx, "ns-b", "acc-1")
	if err != nil {
		t.Fatalf("read ns-b devices: %v", err)
	}
	if !reflect.DeepEqual(other, []string{"device-other"}) {
		t.Fatalf("expected namespace isolation, got %#v", other)
	}

	offline, err := store.AccountDeviceIDs(ctx, "ns-a", "acc-2")
	if err != nil {
		t.Fatalf("read offline account devices: %v", err)
	}
	if len(offline) != 0 {
		t.Fatalf("expected no devices for offline account, got %#v", offline)
	}
}

func TestRedisPresenceStoreRemoveKeepsDevicesWithLiveConnections(t *testing.T) {
	store := newTestPresenceStore(t, time.Minute)
	ctx := context.Background()
	const (
		namespace = "test-ns"
		accountID = "acc-1"
	)

	if err := store.Register(ctx, namespace, accountID, "device-1", "conn-1"); err != nil {
		t.Fatalf("register conn-1: %v", err)
	}
	if err := store.Register(ctx, namespace, accountID, "device-1", "conn-2"); err != nil {
		t.Fatalf("register conn-2: %v", err)
	}
	if err := store.Register(ctx, namespace, accountID, "device-2", "conn-3"); err != nil {
		t.Fatalf("register conn-3: %v", err)
	}

	// Removing one of two connections of a device must not evict the device.
	if err := store.Remove(ctx, namespace, accountID, "device-1", "conn-1"); err != nil {
		t.Fatalf("remove conn-1: %v", err)
	}
	got, err := store.AccountDeviceIDs(ctx, namespace, accountID)
	if err != nil {
		t.Fatalf("read devices after removing conn-1: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"device-1", "device-2"}) {
		t.Fatalf("expected both devices to stay online, got %#v", got)
	}

	// The last connection of a device drops it.
	if err := store.Remove(ctx, namespace, accountID, "device-2", "conn-3"); err != nil {
		t.Fatalf("remove conn-3: %v", err)
	}
	got, err = store.AccountDeviceIDs(ctx, namespace, accountID)
	if err != nil {
		t.Fatalf("read devices after removing conn-3: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"device-1"}) {
		t.Fatalf("expected device-2 to be gone, got %#v", got)
	}

	if err := store.Remove(ctx, namespace, accountID, "device-1", "conn-2"); err != nil {
		t.Fatalf("remove conn-2: %v", err)
	}
	got, err = store.AccountDeviceIDs(ctx, namespace, accountID)
	if err != nil {
		t.Fatalf("read devices after removing conn-2: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the account to be offline, got %#v", got)
	}
}

func TestRedisPresenceStoreAccountDeviceIDsExpire(t *testing.T) {
	store := newTestPresenceStore(t, 50*time.Millisecond)
	ctx := context.Background()
	const namespace = "test-ns"

	if err := store.Register(ctx, namespace, "acc-1", "device-1", "conn-1"); err != nil {
		t.Fatalf("register device: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	got, err := store.AccountDeviceIDs(ctx, namespace, "acc-1")
	if err != nil {
		t.Fatalf("read expired devices: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected expired lease to drop the device, got %#v", got)
	}
}

func TestRedisPresenceStoreAccountsDeviceIDs(t *testing.T) {
	store := newTestPresenceStore(t, time.Minute)
	ctx := context.Background()
	const namespace = "test-ns"

	if err := store.Register(ctx, namespace, "acc-1", "device-1", "conn-1"); err != nil {
		t.Fatalf("register device-1: %v", err)
	}
	if err := store.Register(ctx, namespace, "acc-1", "device-2", "conn-2"); err != nil {
		t.Fatalf("register device-2: %v", err)
	}
	if err := store.Register(ctx, namespace, "acc-2", "device-3", "conn-3"); err != nil {
		t.Fatalf("register device-3: %v", err)
	}

	devices, err := store.AccountsDeviceIDs(ctx, namespace, []string{"acc-2", " acc-1 ", "acc-1", "", "acc-3"})
	if err != nil {
		t.Fatalf("read account devices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("expected an entry per requested account, got %#v", devices)
	}
	if !reflect.DeepEqual(devices["acc-1"], []string{"device-1", "device-2"}) {
		t.Fatalf("expected sorted acc-1 devices, got %#v", devices["acc-1"])
	}
	if !reflect.DeepEqual(devices["acc-2"], []string{"device-3"}) {
		t.Fatalf("expected acc-2 devices, got %#v", devices["acc-2"])
	}
	if len(devices["acc-3"]) != 0 {
		t.Fatalf("expected empty entry for offline account, got %#v", devices["acc-3"])
	}
}

func TestRedisPresenceStoreDeviceQueriesWithoutClient(t *testing.T) {
	ctx := context.Background()
	store := NewRedisPresenceStore(nil, "ws:presence:test:nil-client", time.Minute)

	ids, err := store.AccountDeviceIDs(ctx, "test-ns", "acc-1")
	if err != nil || len(ids) != 0 {
		t.Fatalf("expected no devices without a client, got %#v (%v)", ids, err)
	}
	if ids, err := store.AccountDeviceIDs(ctx, "test-ns", "  "); err != nil || ids != nil {
		t.Fatalf("expected blank account to be ignored, got %#v (%v)", ids, err)
	}

	devices, err := store.AccountsDeviceIDs(ctx, "test-ns", []string{"acc-1", " ", "acc-1", "acc-2"})
	if err != nil {
		t.Fatalf("read batch devices without a client: %v", err)
	}
	if len(devices) != 2 || len(devices["acc-1"]) != 0 || len(devices["acc-2"]) != 0 {
		t.Fatalf("expected an empty entry per requested account, got %#v", devices)
	}

	var nilStore *RedisPresenceStore
	if devices, err := nilStore.AccountsDeviceIDs(ctx, "test-ns", []string{"acc-1"}); err != nil || len(devices) != 1 {
		t.Fatalf("expected nil receiver to answer with an empty entry, got %#v (%v)", devices, err)
	}
}
