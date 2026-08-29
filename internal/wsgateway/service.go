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

type accountPresenceLister interface {
	ActiveAccountIDs(context.Context, string) ([]string, error)
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
	Namespaces        map[string]NamespaceConfig
	DefaultNamespace  string
}

type NamespaceConfig struct {
	KeepAliveInterval time.Duration
	MaxMessageBytes   int64
	AllowedDeviceAlt  map[string]struct{}
}

type connectionKey struct {
	namespace string
	accountID string
	deviceID  string
}

type wsConnection struct {
	connectionID string
	namespace    string
	account      *gen.DyAccount
	sessionID    string
	deviceID     string
	expiresAt    time.Time
	reauthOK     bool
	tokenInfo    dyauth.TokenInfo
	authReq      *http.Request
	conn         *websocket.Conn
	cancel       context.CancelFunc
	done         chan struct{}
	priority     chan Packet
	outbound     chan Packet
	closeOnce    sync.Once
	metaMu       sync.RWMutex
	probe        func() bool
}

type ConnectionSnapshot struct {
	Namespace string `json:"namespace"`
	AccountID string `json:"accountId"`
	DeviceID  string `json:"deviceId"`
}

const (
	closeReasonFlushDelay = 50 * time.Millisecond
	priorityQueueSize     = 16
	outboundQueueSize     = 128
	outboundWriteTimeout  = 5 * time.Second
)

var (
	errWebSocketClosed   = errors.New("websocket connection is closed")
	errOutboundQueueFull = errors.New("websocket outbound queue is full")
)

type Service struct {
	cfg           Config
	handlers      map[string]PacketHandler
	forwarder     UnknownPacketForwarder
	events        ConnectionEventPublisher
	authenticator dyauth.TokenAuthenticator
	cache         cache.CacheService
	profiles      gen.DyProfileServiceClient
	presence      PresenceStore

	mu          sync.RWMutex
	connections map[connectionKey]*wsConnection
}

// SetPresence enables shared connection status. It is optional so a gateway
// can still run in a single-node development environment without Redis.
func (s *Service) SetPresence(presence PresenceStore) { s.presence = presence }

func (s *Service) GetDefaultNamespace() string {
	return s.cfg.DefaultNamespace
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
	if cfg.Namespaces == nil {
		cfg.Namespaces = make(map[string]NamespaceConfig)
	}
	if cfg.DefaultNamespace == "" {
		cfg.DefaultNamespace = "_default"
	}
	if _, ok := cfg.Namespaces[cfg.DefaultNamespace]; !ok {
		cfg.Namespaces[cfg.DefaultNamespace] = NamespaceConfig{
			KeepAliveInterval: cfg.KeepAliveInterval,
			MaxMessageBytes:   cfg.MaxMessageBytes,
			AllowedDeviceAlt:  cfg.AllowedDeviceAlt,
		}
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

func (s *Service) resolveNamespaceConfig(namespace string) NamespaceConfig {
	if ns, ok := s.cfg.Namespaces[namespace]; ok {
		if ns.KeepAliveInterval <= 0 {
			ns.KeepAliveInterval = s.cfg.KeepAliveInterval
		}
		if ns.MaxMessageBytes <= 0 {
			ns.MaxMessageBytes = s.cfg.MaxMessageBytes
		}
		if ns.AllowedDeviceAlt == nil {
			ns.AllowedDeviceAlt = s.cfg.AllowedDeviceAlt
		}
		return ns
	}
	return NamespaceConfig{
		KeepAliveInterval: s.cfg.KeepAliveInterval,
		MaxMessageBytes:   s.cfg.MaxMessageBytes,
		AllowedDeviceAlt:  s.cfg.AllowedDeviceAlt,
	}
}

func (s *Service) TryAdd(auth SessionAuthContext, namespace, deviceID string, conn *websocket.Conn, cancel context.CancelFunc) (*wsConnection, *wsConnection) {
	account := auth.Account
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	key := connectionKey{namespace: namespace, accountID: account.GetId(), deviceID: deviceID}
	reauthOK := supportsSessionReauth(auth.TokenInfo)
	expiresAt := timestampToTime(auth.Session.GetExpiredAt())
	if !reauthOK {
		expiresAt = time.Time{}
	}
	entry := &wsConnection{
		connectionID: uuid.NewString(),
		namespace:    namespace,
		account:      account,
		sessionID:    auth.Session.GetId(),
		deviceID:     deviceID,
		expiresAt:    expiresAt,
		reauthOK:     reauthOK,
		tokenInfo:    auth.TokenInfo,
		authReq:      auth.Request,
		conn:         conn,
		cancel:       cancel,
		done:         make(chan struct{}),
		priority:     make(chan Packet, priorityQueueSize),
		outbound:     make(chan Packet, outboundQueueSize),
	}

	s.mu.Lock()
	old := s.connections[key]
	s.connections[key] = entry
	s.mu.Unlock()

	return entry, old
}

func (s *Service) TryAddUniqueDevice(namespace string, account *gen.DyAccount, deviceID string, conn *websocket.Conn) (*wsConnection, bool) {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	key := connectionKey{namespace: namespace, accountID: account.GetId(), deviceID: deviceID}
	entry := &wsConnection{
		connectionID: uuid.NewString(),
		namespace:    namespace,
		account:      account,
		deviceID:     deviceID,
		conn:         conn,
		done:         make(chan struct{}),
		priority:     make(chan Packet, priorityQueueSize),
		outbound:     make(chan Packet, outboundQueueSize),
	}

	s.mu.Lock()
	for existingKey, existing := range s.connections {
		if existing.namespace != namespace || existing.deviceID != deviceID {
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

func (s *Service) Disconnect(namespace, accountID, deviceID string, reason string) {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	key := connectionKey{namespace: namespace, accountID: accountID, deviceID: deviceID}

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

func (s *Service) DisconnectSession(namespace, sessionID, accountID, deviceID, reason string) int {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	sessionID = strings.TrimSpace(sessionID)
	accountID = strings.TrimSpace(accountID)
	deviceID = strings.TrimSpace(deviceID)

	s.mu.Lock()
	targets := make([]*wsConnection, 0, len(s.connections))
	for key, entry := range s.connections {
		if key.namespace != namespace {
			continue
		}
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
	parsed := ts.AsTime().UTC()
	if !parsed.After(time.Unix(0, 0).UTC()) {
		return time.Time{}
	}
	return parsed
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
				s.Disconnect(entry.namespace, entry.getAccountID(), entry.deviceID, reason)
				return
			}
		}
	}()
}

func (s *Service) startPresenceMonitor(ctx context.Context, entry *wsConnection) {
	if s.presence == nil {
		return
	}
	nsCfg := s.resolveNamespaceConfig(entry.namespace)
	interval := nsCfg.KeepAliveInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.presence.Refresh(ctx, entry.namespace, entry.getAccountID(), entry.deviceID, entry.connectionID); err != nil {
					logging.Log.Warn().Err(err).Str("namespace", entry.namespace).Str("accountId", entry.getAccountID()).Str("deviceId", entry.deviceID).Msg("Failed to refresh websocket presence")
				}
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

func (s *Service) remove(namespace, accountID, deviceID string, expected *wsConnection) {
	key := connectionKey{namespace: namespace, accountID: accountID, deviceID: deviceID}
	s.mu.Lock()
	if current := s.connections[key]; expected == nil || current == expected {
		delete(s.connections, key)
	}
	s.mu.Unlock()
}

func (s *Service) GetDeviceIsConnected(namespace, deviceID string) bool {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	if s.presence != nil {
		if connected, err := s.presence.DeviceConnected(context.Background(), namespace, deviceID); err == nil {
			if connected {
				return true
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, conn := range s.connections {
		if conn.namespace == namespace && conn.deviceID == deviceID {
			return true
		}
	}
	return false
}

func (s *Service) GetConnectedDeviceIDs(namespace string, deviceIDs []string) []string {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	uniqueDeviceIDs := make(map[string]struct{}, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		deviceID = strings.TrimSpace(deviceID)
		if deviceID != "" {
			uniqueDeviceIDs[deviceID] = struct{}{}
		}
	}
	if len(uniqueDeviceIDs) == 0 {
		return nil
	}

	requestedDeviceIDs := make([]string, 0, len(uniqueDeviceIDs))
	for deviceID := range uniqueDeviceIDs {
		requestedDeviceIDs = append(requestedDeviceIDs, deviceID)
	}
	if s.presence != nil {
		if connected, err := s.presence.DevicesConnected(context.Background(), namespace, requestedDeviceIDs); err == nil {
			return connectedDeviceIDs(connected)
		}
	}

	s.mu.RLock()
	connected := make(map[string]bool, len(requestedDeviceIDs))
	for _, conn := range s.connections {
		if conn.namespace == namespace {
			if _, requested := uniqueDeviceIDs[conn.deviceID]; requested {
				connected[conn.deviceID] = true
			}
		}
	}
	s.mu.RUnlock()
	return connectedDeviceIDs(connected)
}

func connectedDeviceIDs(statuses map[string]bool) []string {
	deviceIDs := make([]string, 0, len(statuses))
	for deviceID, connected := range statuses {
		if connected {
			deviceIDs = append(deviceIDs, deviceID)
		}
	}
	sort.Strings(deviceIDs)
	return deviceIDs
}

func (s *Service) GetAccountIsConnected(namespace, accountID string) bool {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	if s.presence != nil {
		if connected, err := s.presence.AccountConnected(context.Background(), namespace, accountID); err == nil {
			if connected {
				return true
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for key := range s.connections {
		if key.namespace == namespace && key.accountID == accountID {
			return true
		}
	}
	return false
}

func (s *Service) GetAllConnectedUserIDs(namespace string) []string {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	if lister, ok := s.presence.(accountPresenceLister); ok {
		if accountIDs, err := lister.ActiveAccountIDs(context.Background(), namespace); err == nil {
			return accountIDs
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]struct{})
	for key := range s.connections {
		if key.namespace == namespace {
			seen[key.accountID] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for accountID := range seen {
		out = append(out, accountID)
	}
	sort.Strings(out)
	return out
}

func (s *Service) GetAllConnectedDeviceIDs(namespace string) []string {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, conn := range s.connections {
		if conn.namespace == namespace {
			seen[conn.deviceID] = struct{}{}
		}
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

func (s *Service) GetDevicesByAccount(namespace, accountID string) []string {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := make([]string, 0)
	for key, conn := range s.connections {
		if key.namespace == namespace && key.accountID == accountID {
			devices = append(devices, conn.deviceID)
		}
	}
	sort.Strings(devices)
	return devices
}

func (s *Service) GetAccountsByDevice(namespace, deviceID string) []string {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]string, 0)
	for key, conn := range s.connections {
		if key.namespace == namespace && conn.deviceID == deviceID {
			accounts = append(accounts, key.accountID)
		}
	}
	sort.Strings(accounts)
	return accounts
}

func (s *Service) SendPacketToAccount(namespace, accountID string, packet *gen.DyWebSocketPacket) {
	s.SendPacketToAccountExcept(namespace, accountID, packet, nil)
}

func (s *Service) SendPacketToAccountExcept(namespace, accountID string, packet *gen.DyWebSocketPacket, excludedDeviceIDs []string) {
	s.SendPacketToAccountsExcept(namespace, []string{accountID}, packet, excludedDeviceIDs)
}

func (s *Service) SendPacketToAccountsExcept(namespace string, accountIDs []string, packet *gen.DyWebSocketPacket, excludedDeviceIDs []string) {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	excluded := makeDeviceIDSet(excludedDeviceIDs)
	targets := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range uniqueTrimmedStrings(accountIDs) {
		targets[accountID] = struct{}{}
	}

	s.mu.RLock()
	entries := make([]*wsConnection, 0)
	for key, entry := range s.connections {
		if key.namespace != namespace {
			continue
		}
		if _, ok := targets[key.accountID]; !ok {
			continue
		}
		if _, skip := excluded[entry.deviceID]; skip {
			continue
		}
		entries = append(entries, entry)
	}
	s.mu.RUnlock()

	logging.Log.Debug().
		Str("namespace", namespace).
		Str("packetType", packet.GetType()).
		Strs("accountIds", uniqueTrimmedStrings(accountIDs)).
		Strs("excludedDeviceIds", excludedDeviceIDs).
		Int("targetCount", len(entries)).
		Msg("Selected websocket account push targets")

	for _, entry := range entries {
		logging.Log.Debug().
			Str("namespace", namespace).
			Str("accountId", entry.getAccountID()).
			Str("deviceId", entry.deviceID).
			Str("packetType", packet.GetType()).
			Msg("Sending websocket packet to account connection")
		if err := entry.sendProto(packet); err != nil {
			logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", entry.getAccountID()).Str("deviceId", entry.deviceID).Msg("Failed to send packet to account connection")
		}
	}
}

func (s *Service) getAccountConnections(namespace, accountID string, excludedDeviceIDs []string) []*wsConnection {
	excluded := makeDeviceIDSet(excludedDeviceIDs)
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]*wsConnection, 0)
	for key, entry := range s.connections {
		if key.namespace == namespace && key.accountID == accountID {
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

func (s *Service) SendPacketToDevice(namespace, deviceID string, packet *gen.DyWebSocketPacket) {
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	entries := s.getDeviceConnections(namespace, deviceID)

	logging.Log.Debug().
		Str("namespace", namespace).
		Str("deviceId", deviceID).
		Str("packetType", packet.GetType()).
		Int("targetCount", len(entries)).
		Msg("Selected websocket device push targets")

	for _, entry := range entries {
		logging.Log.Debug().
			Str("namespace", namespace).
			Str("accountId", entry.getAccountID()).
			Str("deviceId", deviceID).
			Str("packetType", packet.GetType()).
			Msg("Sending websocket packet to device connection")
		if err := entry.sendProtoPriority(packet); err != nil {
			logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", entry.getAccountID()).Str("deviceId", deviceID).Msg("Failed to send packet to device")
		}
	}
}

func (s *Service) getDeviceConnections(namespace, deviceID string) []*wsConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]*wsConnection, 0)
	for _, entry := range s.connections {
		if entry.namespace == namespace && entry.deviceID == deviceID {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (s *Service) HandlePacket(ctx context.Context, account *gen.DyAccount, namespace, deviceID string, packet Packet) error {
	if packet.Type == "" {
		return errors.New("empty packet type")
	}

	if packet.Type == PacketTypePing {
		s.SendPacketToDevice(namespace, deviceID, &gen.DyWebSocketPacket{Type: PacketTypePong})
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

func (s *Service) HandleConnection(ctx context.Context, auth SessionAuthContext, namespace, deviceID string, conn *websocket.Conn) {
	account := auth.Account
	if namespace == "" {
		namespace = s.cfg.DefaultNamespace
	}
	deviceID = s.normalizeDeviceID(deviceID)
	connCtx, cancel := context.WithCancel(ctx)

	logging.Log.Info().
		Str("namespace", namespace).
		Str("accountId", account.GetId()).
		Str("deviceId", deviceID).
		Str("sessionId", auth.Session.GetId()).
		Msg("Handling websocket connection")

	entry, old := s.TryAdd(auth, namespace, deviceID, conn, cancel)
	entry.startWriter()
	if s.presence != nil {
		if err := s.presence.Register(connCtx, namespace, account.GetId(), deviceID, entry.connectionID); err != nil {
			logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to register websocket presence")
		} else {
			s.startPresenceMonitor(connCtx, entry)
		}
	}
	if old != nil {
		logging.Log.Info().
			Str("namespace", namespace).
			Str("accountId", account.GetId()).
			Str("deviceId", deviceID).
			Msg("Disconnecting previous websocket connection due to duplicated device id")
		old.close("connection replaced by new client")
	}

	s.startSessionMonitor(connCtx, entry)

	if s.events != nil {
		if err := s.events.PublishConnected(connCtx, account.GetId(), deviceID); err != nil {
			logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket connect event")
		}
	}

	defer func() {
		cancel()
		s.remove(namespace, account.GetId(), deviceID, entry)
		if s.presence != nil {
			if err := s.presence.Remove(context.Background(), namespace, account.GetId(), deviceID, entry.connectionID); err != nil {
				logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to remove websocket presence")
			}
		}
		isOffline := !s.GetAccountIsConnected(namespace, account.GetId())
		if s.events != nil {
			if err := s.events.PublishDisconnected(connCtx, account.GetId(), deviceID, isOffline); err != nil {
				logging.Log.Warn().Err(err).Str("namespace", namespace).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket disconnect event")
			}
		}
		entry.close("")
		logging.Log.Info().
			Str("namespace", namespace).
			Str("accountId", account.GetId()).
			Str("deviceId", deviceID).
			Str("sessionId", entry.getSessionID()).
			Bool("isOffline", isOffline).
			Msg("Websocket connection closed")
	}()

	nsCfg := s.resolveNamespaceConfig(namespace)

	for {
		var raw []byte
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			logging.Log.Debug().
				Err(err).
				Str("namespace", namespace).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Stopped websocket receive loop")
			return
		}
		if int64(len(raw)) > nsCfg.MaxMessageBytes {
			logging.Log.Warn().
				Int("sizeBytes", len(raw)).
				Int64("maxMessageBytes", nsCfg.MaxMessageBytes).
				Str("namespace", namespace).
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
				Str("namespace", namespace).
				Str("accountId", account.GetId()).
				Str("deviceId", deviceID).
				Msg("Rejected websocket packet due to invalid JSON")
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "unprocessable packet: invalid json"})
			continue
		}

		if err := s.HandlePacket(connCtx, account, namespace, deviceID, packet); err != nil {
			logging.Log.Warn().
				Err(err).
				Str("packetType", packet.Type).
				Str("endpoint", packet.Endpoint).
				Str("namespace", namespace).
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

func (c *wsConnection) sendProtoPriority(packet *gen.DyWebSocketPacket) error {
	return c.sendJSONPriority(packetFromProto(packet))
}

func (c *wsConnection) sendJSON(packet Packet) error {
	return c.enqueue(packet, c.outbound)
}

func (c *wsConnection) sendJSONPriority(packet Packet) error {
	return c.enqueue(packet, c.priority)
}

func (c *wsConnection) enqueue(packet Packet, queue chan<- Packet) error {
	if c.conn == nil {
		return errors.New("websocket connection is nil")
	}
	if queue == nil {
		return errors.New("websocket outbound queue is nil")
	}

	select {
	case <-c.done:
		return errWebSocketClosed
	default:
	}

	select {
	case queue <- packet:
		return nil
	default:
		go c.close("websocket outbound queue is full")
		return errOutboundQueueFull
	}
}

func (c *wsConnection) startWriter() {
	if c == nil || c.priority == nil || c.outbound == nil {
		return
	}

	go func() {
		for {
			select {
			case <-c.done:
				return
			case packet := <-c.priority:
				if !c.writeQueuedPacket(packet) {
					return
				}
			default:
			}

			select {
			case <-c.done:
				return
			case packet := <-c.priority:
				if !c.writeQueuedPacket(packet) {
					return
				}
			case packet := <-c.outbound:
				if !c.writeQueuedPacket(packet) {
					return
				}
			}
		}
	}()
}

func (c *wsConnection) writeQueuedPacket(packet Packet) bool {
	if err := c.writeJSON(packet); err != nil {
		logging.Log.Warn().
			Err(err).
			Str("accountId", c.getAccountID()).
			Str("deviceId", c.deviceID).
			Msg("Closing websocket connection after write failure")
		c.close("")
		return false
	}
	return true
}

func (c *wsConnection) writeJSON(packet Packet) error {
	payload, err := json.Marshal(packet)
	if err != nil {
		return err
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(outboundWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}()

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
