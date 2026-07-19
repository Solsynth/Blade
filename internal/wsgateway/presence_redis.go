package wsgateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// PresenceStore holds only routable connection metadata. Socket objects always
// remain local to the gateway that accepted the WebSocket.
type PresenceStore interface {
	Register(context.Context, string, string, string) error
	Refresh(context.Context, string, string, string) error
	Remove(context.Context, string, string, string) error
	AccountConnected(context.Context, string) (bool, error)
	DeviceConnected(context.Context, string) (bool, error)
	DevicesConnected(context.Context, []string) (map[string]bool, error)
}

type RedisPresenceStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

func NewRedisPresenceStore(client *redis.Client, prefix string, ttl time.Duration) *RedisPresenceStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "ws:presence"
	}
	return &RedisPresenceStore{client: client, prefix: prefix, ttl: ttl}
}

func (p *RedisPresenceStore) accountKey(accountID string) string {
	return p.prefix + ":account:" + accountID
}
func (p *RedisPresenceStore) deviceKey(deviceID string) string {
	return p.prefix + ":device:" + deviceID
}
func (p *RedisPresenceStore) accountsKey() string { return p.prefix + ":accounts" }

func (p *RedisPresenceStore) Register(ctx context.Context, accountID, deviceID, connectionID string) error {
	return p.touch(ctx, accountID, deviceID, connectionID)
}

func (p *RedisPresenceStore) Refresh(ctx context.Context, accountID, deviceID, connectionID string) error {
	return p.touch(ctx, accountID, deviceID, connectionID)
}

func (p *RedisPresenceStore) touch(ctx context.Context, accountID, deviceID, connectionID string) error {
	if p == nil || p.client == nil {
		return nil
	}
	member := strings.TrimSpace(connectionID)
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(deviceID) == "" || member == "" {
		return fmt.Errorf("account, device, and connection IDs are required")
	}
	expiresAt := float64(time.Now().Add(p.ttl).UnixMilli())
	pipe := p.client.Pipeline()
	pipe.ZAdd(ctx, p.accountKey(accountID), redis.Z{Score: expiresAt, Member: member})
	pipe.ZAdd(ctx, p.deviceKey(deviceID), redis.Z{Score: expiresAt, Member: member})
	pipe.ZAdd(ctx, p.accountsKey(), redis.Z{Score: expiresAt, Member: accountID})
	// The scores are the authoritative per-connection lease. Key expiry also
	// bounds Redis memory for identities that are never queried again.
	pipe.Expire(ctx, p.accountKey(accountID), 2*p.ttl)
	pipe.Expire(ctx, p.deviceKey(deviceID), 2*p.ttl)
	pipe.Expire(ctx, p.accountsKey(), 2*p.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (p *RedisPresenceStore) Remove(ctx context.Context, accountID, deviceID, connectionID string) error {
	if p == nil || p.client == nil {
		return nil
	}
	pipe := p.client.Pipeline()
	pipe.ZRem(ctx, p.accountKey(accountID), connectionID)
	pipe.ZRem(ctx, p.deviceKey(deviceID), connectionID)
	_, err := pipe.Exec(ctx)
	return err
}

func (p *RedisPresenceStore) AccountConnected(ctx context.Context, accountID string) (bool, error) {
	return p.connected(ctx, p.accountKey(accountID))
}

func (p *RedisPresenceStore) DeviceConnected(ctx context.Context, deviceID string) (bool, error) {
	return p.connected(ctx, p.deviceKey(deviceID))
}

func (p *RedisPresenceStore) DevicesConnected(ctx context.Context, deviceIDs []string) (map[string]bool, error) {
	connected := make(map[string]bool, len(deviceIDs))
	if p == nil || p.client == nil {
		return connected, nil
	}

	now := time.Now().UnixMilli()
	pipe := p.client.TxPipeline()
	counts := make(map[string]*redis.IntCmd, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		deviceID = strings.TrimSpace(deviceID)
		if deviceID == "" {
			continue
		}
		if _, exists := counts[deviceID]; exists {
			continue
		}
		key := p.deviceKey(deviceID)
		pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", now))
		counts[deviceID] = pipe.ZCount(ctx, key, fmt.Sprintf("(%d", now), "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for deviceID, count := range counts {
		connected[deviceID] = count.Val() > 0
	}
	return connected, nil
}

func (p *RedisPresenceStore) ActiveAccountIDs(ctx context.Context) ([]string, error) {
	if p == nil || p.client == nil {
		return nil, nil
	}
	now := time.Now().UnixMilli()
	if err := p.client.ZRemRangeByScore(ctx, p.accountsKey(), "-inf", fmt.Sprintf("%d", now)).Err(); err != nil {
		return nil, err
	}
	accountIDs, err := p.client.ZRangeByScore(ctx, p.accountsKey(), &redis.ZRangeBy{Min: fmt.Sprintf("(%d", now), Max: "+inf"}).Result()
	if err != nil {
		return nil, err
	}
	active := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		connected, err := p.AccountConnected(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if connected {
			active = append(active, accountID)
		} else {
			_ = p.client.ZRem(ctx, p.accountsKey(), accountID).Err()
		}
	}
	sort.Strings(active)
	return active, nil
}

func (p *RedisPresenceStore) connected(ctx context.Context, key string) (bool, error) {
	if p == nil || p.client == nil {
		return false, nil
	}
	now := time.Now().UnixMilli()
	pipe := p.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", now))
	count := pipe.ZCount(ctx, key, fmt.Sprintf("(%d", now), "+inf")
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	return count.Val() > 0, nil
}
