package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	"src.solsynth.dev/sosys/go/pkg/cache"
	gen "src.solsynth.dev/sosys/go/proto"
	"srv.solsynth.dev/sosys/blade/internal/logging"
)

type PacketHandler interface {
	PacketType() string
	Handle(ctx context.Context, account *gen.DyAccount, deviceID string, packet *gen.DyWebSocketPacket, svc *Service) error
}

type UnknownPacketForwarder interface {
	Forward(ctx context.Context, account *gen.DyAccount, deviceID string, endpoint string, packet *gen.DyWebSocketPacket) error
}

type ConnectionEventPublisher interface {
	PublishConnected(ctx context.Context, accountID string, deviceID string) error
	PublishDisconnected(ctx context.Context, accountID string, deviceID string, isOffline bool) error
}

type SessionAuthContext struct {
	Account   *gen.DyAccount
	Session   *gen.DyAuthSession
	TokenInfo dyauth.TokenInfo
	Request   *http.Request
}

type Config struct {
	KeepAliveInterval time.Duration
	MaxMessageBytes   int64
	AllowedDeviceAlt  map[string]struct{}
}

type connectionKey struct {
	accountID string
	deviceID  string
}

type wsConnection struct {
	account   *gen.DyAccount
	sessionID string
	deviceID  string
	expiresAt time.Time
	reauthOK  bool
	tokenInfo dyauth.TokenInfo
	authReq   *http.Request
	conn      *websocket.Conn
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	metaMu    sync.RWMutex
	probe     func() bool
}

type ConnectionSnapshot struct {
	AccountID string `json:"accountId"`
	DeviceID  string `json:"deviceId"`
}

var closeReasonFlushDelay = 50 * time.Millisecond

type Service struct {
	cfg           Config
	handlers      map[string]PacketHandler
	forwarder     UnknownPacketForwarder
	events        ConnectionEventPublisher
	authenticator dyauth.TokenAuthenticator
	cache         cache.CacheService
	profiles      gen.DyProfileServiceClient

	mu          sync.RWMutex
	connections map[connectionKey]*wsConnection
}

func NewService(cfg Config, handlers []PacketHandler, forwarder UnknownPacketForwarder, events ConnectionEventPublisher, authenticator dyauth.TokenAuthenticator, c cache.CacheService, profiles gen.DyProfileServiceClient) *Service {
	handlerMap := make(map[string]PacketHandler, len(handlers))
	for _, h := range handlers {
		handlerMap[h.PacketType()] = h
	}

	if cfg.KeepAliveInterval <= 0 {
		cfg.KeepAliveInterval = 60 * time.Second
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = 4 * 1024
	}
	if cfg.AllowedDeviceAlt == nil {
		cfg.AllowedDeviceAlt = map[string]struct{}{"watch": {}}
	}

	return &Service{
		cfg:           cfg,
		handlers:      handlerMap,
		forwarder:     forwarder,
		events:        events,
		authenticator: authenticator,
		cache:         c,
		profiles:      profiles,
		connections:   make(map[connectionKey]*wsConnection),
	}
}

func (s *Service) TryAdd(auth SessionAuthContext, deviceID string, conn *websocket.Conn, cancel context.CancelFunc) (*wsConnection, *wsConnection) {
	account := auth.Account
	key := connectionKey{accountID: account.GetId(), deviceID: deviceID}
	reauthOK := supportsSessionReauth(auth.TokenInfo)
	expiresAt := timestampToTime(auth.Session.GetExpiredAt())
	if !reauthOK {
		expiresAt = time.Time{}
	}
	entry := &wsConnection{
		account:   account,
		sessionID: auth.Session.GetId(),
		deviceID:  deviceID,
		expiresAt: expiresAt,
		reauthOK:  reauthOK,
		tokenInfo: auth.TokenInfo,
		authReq:   auth.Request,
		conn:      conn,
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	s.mu.Lock()
	old := s.connections[key]
	s.connections[key] = entry
	s.mu.Unlock()

	return entry, old
}

func (s *Service) TryAddUniqueDevice(account *gen.DyAccount, deviceID string, conn *websocket.Conn) (*wsConnection, bool) {
	key := connectionKey{accountID: account.GetId(), deviceID: deviceID}
	entry := &wsConnection{account: account, deviceID: deviceID, conn: conn, done: make(chan struct{})}

	s.mu.Lock()
	for existingKey, existing := range s.connections {
		if existing.deviceID != deviceID {
			continue
		}

		s.mu.Unlock()
		if existing.isAlive() {
			return nil, false
		}

		s.mu.Lock()
		current, ok := s.connections[existingKey]
		if ok && current == existing {
			delete(s.connections, existingKey)
		}
		break
	}
	s.connections[key] = entry
	s.mu.Unlock()
	return entry, true
}

func (s *Service) Disconnect(accountID, deviceID string, reason string) {
	key := connectionKey{accountID: accountID, deviceID: deviceID}

	s.mu.Lock()
	entry, ok := s.connections[key]
	if ok {
		delete(s.connections, key)
	}
	s.mu.Unlock()

	if !ok {
		return
	}

	entry.close(reason)
}

func (s *Service) DisconnectSession(sessionID, accountID, deviceID, reason string) int {
	sessionID = strings.TrimSpace(sessionID)
	accountID = strings.TrimSpace(accountID)
	deviceID = strings.TrimSpace(deviceID)

	s.mu.Lock()
	targets := make([]*wsConnection, 0, len(s.connections))
	for key, entry := range s.connections {
		if sessionID != "" && entry.getSessionID() == sessionID {
			targets = append(targets, entry)
			delete(s.connections, key)
			continue
		}
		if sessionID == "" && accountID != "" && key.accountID == accountID && deviceID != "" && deviceIDMatches(entry.deviceID, deviceID) {
			targets = append(targets, entry)
			delete(s.connections, key)
		}
	}
	s.mu.Unlock()

	for _, entry := range targets {
		entry.close(reason)
	}
	return len(targets)
}

func deviceIDMatches(activeDeviceID, revokedDeviceID string) bool {
	activeDeviceID = strings.TrimSpace(activeDeviceID)
	revokedDeviceID = strings.TrimSpace(revokedDeviceID)
	if activeDeviceID == "" || revokedDeviceID == "" {
		return false
	}
	return activeDeviceID == revokedDeviceID || strings.HasPrefix(activeDeviceID, revokedDeviceID+"+")
}

func timestampToTime(ts interface{ AsTime() time.Time }) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

func (s *Service) startSessionMonitor(ctx context.Context, entry *wsConnection) {
	if s.authenticator == nil || !entry.supportsReauth() {
		return
	}

	go func() {
		for {
			waitFor, ok := entry.nextReauthWait()
			if !ok {
				return
			}

			timer := time.NewTimer(waitFor)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}

			if err := s.reauthenticateConnection(ctx, entry); err != nil {
				reason := fmt.Sprintf("reauthentication failed: %v", err)
				logging.Log.Warn().
					Err(err).
					Str("accountId", entry.getAccountID()).
					Str("deviceId", entry.deviceID).
					Str("sessionId", entry.getSessionID()).
					Str("type", string(entry.tokenInfo.Type)).
					Str("disconnectReason", reason).
					Msg("Disconnecting websocket connection after session reauthentication failure")
				s.Disconnect(entry.getAccountID(), entry.deviceID, reason)
				return
			}
		}
	}()
}

func supportsSessionReauth(tokenInfo dyauth.TokenInfo) bool {
	if strings.TrimSpace(tokenInfo.Token) == "" {
		return false
	}

	switch {
	case isAPIKeyTokenInfo(tokenInfo):
		return false
	default:
		return true
	}
}

func isAPIKeyTokenInfo(tokenInfo dyauth.TokenInfo) bool {
	if tokenInfo.Type == dyauth.TokenTypeApiKey {
		return true
	}

	return dyauth.IsApiKeyToken(strings.TrimSpace(tokenInfo.Token))
}

func (s *Service) reauthenticateConnection(ctx context.Context, entry *wsConnection) error {
	if isAPIKeyTokenInfo(entry.tokenInfo) {
		// Skip reauthentication for API keys
		return nil
	}
	authReq := dyauth.CloneRequestMetadata(entry.authReq)
	result, err := dyauth.Reauthenticate(ctx, s.authenticator, entry.tokenInfo, authReq)
	if err != nil {
		return err
	}
	if result == nil || result.Account == nil || result.Session == nil {
		return errors.New("reauthentication returned incomplete session data")
	}
	if strings.TrimSpace(result.Account.GetId()) != entry.getAccountID() {
		return fmt.Errorf("reauthenticated account mismatch: got %s", result.Account.GetId())
	}

	newExpiry := timestampToTime(result.Session.GetExpiredAt())
	if !newExpiry.IsZero() && !newExpiry.After(time.Now().UTC()) {
		if entry.tokenInfo.Type == dyauth.TokenTypeApiKey || result.Session.Type == gen.DySessionType_DY_API_KEY {
			logging.Log.Debug().
				Str("accountId", entry.getAccountID()).
				Str("deviceId", entry.deviceID).
				Str("sessionId", entry.getSessionID()).
				Str("tokenType", string(entry.tokenInfo.Type)).
				Int32("sessionType", int32(result.Session.Type.Number())).
				Msg("Skip reauth session expiry validation due to it's API key")
		} else {
			return errors.New("reauthenticated session is already expired")
		}
	}

	entry.updateSession(result.Account, result.Session)

	// Hydrate profile and touch last-seen after reauthentication
	_ = dyauth.HydrateAndTouch(ctx, s.cache, s.profiles, result)

	logging.Log.Debug().
		Str("accountId", result.Account.GetId()).
		Str("deviceId", entry.deviceID).
		Str("sessionId", result.Session.GetId()).
		Time("expiredAt", newExpiry).
		Msg("Reauthenticated websocket session")
	return nil
}

func (s *Service) remove(accountID, deviceID string) {
	key := connectionKey{accountID: accountID, deviceID: deviceID}
	s.mu.Lock()
	delete(s.connections, key)
	s.mu.Unlock()
}

func (s *Service) GetDeviceIsConnected(deviceID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, conn := range s.connections {
		if conn.deviceID == deviceID {
			return true
		}
	}
	return false
}

func (s *Service) GetAccountIsConnected(accountID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for key := range s.connections {
		if key.accountID == accountID {
			return true
		}
	}
	return false
}

func (s *Service) GetAllConnectedUserIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]struct{})
	for key := range s.connections {
		seen[key.accountID] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for accountID := range seen {
		out = append(out, accountID)
	}
	sort.Strings(out)
	return out
}

func (s *Service) GetAllConnectedDeviceIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, conn := range s.connections {
		seen[conn.deviceID] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for deviceID := range seen {
		out = append(out, deviceID)
	}
	sort.Strings(out)
	return out
}

func (s *Service) GetConnectionSnapshots() []ConnectionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ConnectionSnapshot, 0, len(s.connections))
	for key, conn := range s.connections {
		out = append(out, ConnectionSnapshot{
			AccountID: key.accountID,
			DeviceID:  conn.deviceID,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountID == out[j].AccountID {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].AccountID < out[j].AccountID
	})

	return out
}

func (s *Service) GetDevicesByAccount(accountID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := make([]string, 0)
	for key, conn := range s.connections {
		if key.accountID == accountID {
			devices = append(devices, conn.deviceID)
		}
	}
	sort.Strings(devices)
	return devices
}

func (s *Service) GetAccountsByDevice(deviceID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]string, 0)
	for key, conn := range s.connections {
		if conn.deviceID == deviceID {
			accounts = append(accounts, key.accountID)
		}
	}
	sort.Strings(accounts)
	return accounts
}

func (s *Service) SendPacketToAccount(accountID string, packet *gen.DyWebSocketPacket) {
	s.SendPacketToAccountExcept(accountID, packet, nil)
}

func (s *Service) SendPacketToAccountExcept(accountID string, packet *gen.DyWebSocketPacket, excludedDeviceIDs []string) {
	entries := s.getAccountConnections(accountID, excludedDeviceIDs)
	excluded := uniqueTrimmedStrings(excludedDeviceIDs)

	logging.Log.Debug().
		Str("accountId", accountID).
		Str("packetType", packet.GetType()).
		Strs("excludedDeviceIds", excluded).
		Int("targetCount", len(entries)).
		Msg("Selected websocket account push targets")

	for _, entry := range entries {
		logging.Log.Debug().
			Str("accountId", accountID).
			Str("deviceId", entry.deviceID).
			Str("packetType", packet.GetType()).
			Msg("Sending websocket packet to account connection")
		if err := entry.sendProto(packet); err != nil {
			logging.Log.Warn().Err(err).Str("accountId", accountID).Str("deviceId", entry.deviceID).Msg("Failed to send packet to account connection")
		}
	}
}

func (s *Service) getAccountConnections(accountID string, excludedDeviceIDs []string) []*wsConnection {
	excluded := makeDeviceIDSet(excludedDeviceIDs)
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]*wsConnection, 0)
	for key, entry := range s.connections {
		if key.accountID == accountID {
			if _, skip := excluded[entry.deviceID]; skip {
				continue
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

func makeDeviceIDSet(deviceIDs []string) map[string]struct{} {
	if len(deviceIDs) == 0 {
		return nil
	}

	uniqueDeviceIDs := uniqueTrimmedStrings(deviceIDs)
	out := make(map[string]struct{}, len(uniqueDeviceIDs))
	for _, deviceID := range uniqueDeviceIDs {
		out[deviceID] = struct{}{}
	}
	return out
}

func (s *Service) SendPacketToDevice(deviceID string, packet *gen.DyWebSocketPacket) {
	entries := s.getDeviceConnections(deviceID)

	logging.Log.Debug().
		Str("deviceId", deviceID).
		Str("packetType", packet.GetType()).
		Int("targetCount", len(entries)).
		Msg("Selected websocket device push targets")

	for _, entry := range entries {
		logging.Log.Debug().
			Str("accountId", entry.getAccountID()).
			Str("deviceId", deviceID).
			Str("packetType", packet.GetType()).
			Msg("Sending websocket packet to device connection")
		if err := entry.sendProto(packet); err != nil {
			logging.Log.Warn().Err(err).Str("accountId", entry.getAccountID()).Str("deviceId", deviceID).Msg("Failed to send packet to device")
		}
	}
}

func (s *Service) getDeviceConnections(deviceID string) []*wsConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]*wsConnection, 0)
	for _, entry := range s.connections {
		if entry.deviceID == deviceID {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (s *Service) HandlePacket(ctx context.Context, account *gen.DyAccount, deviceID string, packet Packet) error {
	if packet.Type == "" {
		return errors.New("empty packet type")
	}

	if packet.Type == PacketTypePing {
		s.SendPacketToDevice(deviceID, &gen.DyWebSocketPacket{Type: PacketTypePong})
		return nil
	}

	protoPacket := packetToProto(packet)
	if handler, ok := s.handlers[packet.Type]; ok {
		return handler.Handle(ctx, account, deviceID, protoPacket, s)
	}

	if packet.Endpoint != "" {
		if s.forwarder == nil {
			return fmt.Errorf("no forwarder configured for endpoint %s", packet.Endpoint)
		}
		return s.forwarder.Forward(ctx, account, deviceID, packet.Endpoint, protoPacket)
	}

	return fmt.Errorf("unprocessable packet: %s", packet.Type)
}

func (s *Service) HandleConnection(ctx context.Context, auth SessionAuthContext, deviceID string, conn *websocket.Conn) {
	account := auth.Account
	deviceID = s.normalizeDeviceID(deviceID)
	connCtx, cancel := context.WithCancel(ctx)

	logging.Log.Info().
		Str("accountId", account.GetId()).
		Str("deviceId", deviceID).
		Str("sessionId", auth.Session.GetId()).
		Msg("Handling websocket connection")

	entry, old := s.TryAdd(auth, deviceID, conn, cancel)
	if old != nil {
		logging.Log.Info().
			Str("accountId", account.GetId()).
			Str("deviceId", deviceID).
			Msg("Disconnecting previous websocket connection due to duplicated device id")
		old.close("connection replaced by new client")
	}

	s.startSessionMonitor(connCtx, entry)

	if s.events != nil {
		if err := s.events.PublishConnected(connCtx, account.GetId(), deviceID); err != nil {
			logging.Log.Warn().Err(err).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket connect event")
		}
	}

	defer func() {
		cancel()
		s.remove(account.GetId(), deviceID)
		isOffline := !s.GetAccountIsConnected(account.GetId())
		if s.events != nil {
			if err := s.events.PublishDisconnected(connCtx, account.GetId(), deviceID, isOffline); err != nil {
				logging.Log.Warn().Err(err).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket disconnect event")
			}
		}
		entry.close("")
		logging.Log.Info().
			Str("accountId", account.GetId()).
			Str("deviceId", deviceID).
			Str("sessionId", entry.getSessionID()).
			Bool("isOffline", isOffline).
			Msg("Websocket connection closed")
	}()

	for {
		var raw []byte
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			logging.Log.Debug().
				Err(err).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Stopped websocket receive loop")
			return
		}
		if int64(len(raw)) > s.cfg.MaxMessageBytes {
			logging.Log.Warn().
				Int("sizeBytes", len(raw)).
				Int64("maxMessageBytes", s.cfg.MaxMessageBytes).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Rejected websocket packet due to size limit")
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "message too large"})
			continue
		}

		var packet Packet
		if err := json.Unmarshal(raw, &packet); err != nil {
			logging.Log.Warn().
				Err(err).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Rejected websocket packet due to invalid JSON")
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "unprocessable packet: invalid json"})
			continue
		}

		if err := s.HandlePacket(connCtx, account, deviceID, packet); err != nil {
			logging.Log.Warn().
				Err(err).
				Str("packetType", packet.Type).
				Str("endpoint", packet.Endpoint).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Failed to handle websocket packet")
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: err.Error()})
		}
	}
}

func (s *Service) normalizeDeviceID(deviceID string) string {
	trimmed := strings.TrimSpace(deviceID)
	if trimmed != "" && !strings.HasPrefix(trimmed, "+") {
		return trimmed
	}

	suffix := ""
	if strings.HasPrefix(trimmed, "+") {
		suffix = trimmed
	}

	generated := uuid.NewString() + suffix
	logging.Log.Warn().Str("deviceId", generated).Msg("Missing websocket client_id; generated UUID fallback")
	return generated
}

func (c *wsConnection) sendProto(packet *gen.DyWebSocketPacket) error {
	return c.sendJSON(packetFromProto(packet))
}

func (c *wsConnection) sendJSON(packet Packet) error {
	if c.conn == nil {
		return errors.New("websocket connection is nil")
	}

	payload, err := json.Marshal(packet)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return websocket.Message.Send(c.conn, payload)
}

func (c *wsConnection) close(reason string) {
	if c == nil {
		return
	}

	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if reason != "" {
			_ = c.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: reason})
			time.Sleep(closeReasonFlushDelay)
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if c.done != nil {
			close(c.done)
		}
	})
}

func (c *wsConnection) getAccountID() string {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	if c.account == nil {
		return ""
	}
	return c.account.GetId()
}

func (c *wsConnection) getSessionID() string {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	return strings.TrimSpace(c.sessionID)
}

func (c *wsConnection) nextReauthWait() (time.Duration, bool) {
	if c == nil {
		return 0, false
	}

	c.metaMu.RLock()
	reauthOK := c.reauthOK
	expiresAt := c.expiresAt
	done := c.done
	c.metaMu.RUnlock()

	if !reauthOK {
		return 0, false
	}

	if done != nil {
		select {
		case <-done:
			return 0, false
		default:
		}
	}

	if expiresAt.IsZero() {
		return 0, false
	}

	waitFor := time.Until(expiresAt)
	if waitFor < 0 {
		waitFor = 0
	}
	return waitFor, true
}

func (c *wsConnection) updateSession(account *gen.DyAccount, session *gen.DyAuthSession) {
	if c == nil || session == nil {
		return
	}

	c.metaMu.Lock()
	if account != nil {
		c.account = account
	}
	c.sessionID = session.GetId()
	c.expiresAt = timestampToTime(session.GetExpiredAt())
	c.metaMu.Unlock()
}

func (c *wsConnection) supportsReauth() bool {
	if c == nil {
		return false
	}

	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	return c.reauthOK
}

func (c *wsConnection) isAlive() bool {
	if c != nil && c.probe != nil {
		return c.probe()
	}
	if c == nil || c.conn == nil {
		return false
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return false
	}
	defer func() {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}()

	return c.sendJSON(Packet{Type: PacketTypePing}) == nil
}
