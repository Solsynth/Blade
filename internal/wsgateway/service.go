package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"git.solsynth.dev/solarnetwork/blade/internal/logging"
	gen "git.solsynth.dev/solarnetwork/dysonproto/gen/go"
	"golang.org/x/net/websocket"
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
	account  *gen.DyAccount
	deviceID string
	conn     *websocket.Conn
	mu       sync.Mutex
}

type Service struct {
	cfg       Config
	handlers  map[string]PacketHandler
	forwarder UnknownPacketForwarder
	events    ConnectionEventPublisher

	mu          sync.RWMutex
	connections map[connectionKey]*wsConnection
}

func NewService(cfg Config, handlers []PacketHandler, forwarder UnknownPacketForwarder, events ConnectionEventPublisher) *Service {
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
		cfg:         cfg,
		handlers:    handlerMap,
		forwarder:   forwarder,
		events:      events,
		connections: make(map[connectionKey]*wsConnection),
	}
}

func (s *Service) TryAdd(account *gen.DyAccount, deviceID string, conn *websocket.Conn) (*wsConnection, *wsConnection) {
	key := connectionKey{accountID: account.GetId(), deviceID: deviceID}
	entry := &wsConnection{account: account, deviceID: deviceID, conn: conn}

	s.mu.Lock()
	old := s.connections[key]
	s.connections[key] = entry
	s.mu.Unlock()

	return entry, old
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

	if reason != "" {
		_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: reason})
	}
	_ = entry.conn.Close()
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
	return out
}

func (s *Service) SendPacketToAccount(accountID string, packet *gen.DyWebSocketPacket) {
	s.mu.RLock()
	entries := make([]*wsConnection, 0)
	for key, entry := range s.connections {
		if key.accountID == accountID {
			entries = append(entries, entry)
		}
	}
	s.mu.RUnlock()

	for _, entry := range entries {
		if err := entry.sendProto(packet); err != nil {
			logging.Log.Warn().Err(err).Str("accountId", accountID).Str("deviceId", entry.deviceID).Msg("Failed to send packet to account connection")
		}
	}
}

func (s *Service) SendPacketToDevice(deviceID string, packet *gen.DyWebSocketPacket) {
	s.mu.RLock()
	entries := make([]*wsConnection, 0)
	for _, entry := range s.connections {
		if entry.deviceID == deviceID {
			entries = append(entries, entry)
		}
	}
	s.mu.RUnlock()

	for _, entry := range entries {
		if err := entry.sendProto(packet); err != nil {
			logging.Log.Warn().Err(err).Str("deviceId", deviceID).Msg("Failed to send packet to device")
		}
	}
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

func (s *Service) HandleConnection(ctx context.Context, account *gen.DyAccount, deviceID string, conn *websocket.Conn) {
	if s.events != nil {
		if err := s.events.PublishConnected(ctx, account.GetId(), deviceID); err != nil {
			logging.Log.Warn().Err(err).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket connect event")
		}
	}

	entry, old := s.TryAdd(account, deviceID, conn)
	if old != nil {
		_ = old.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "Just connected somewhere else..."})
		_ = old.conn.Close()
	}

	defer func() {
		s.remove(account.GetId(), deviceID)
		isOffline := !s.GetAccountIsConnected(account.GetId())
		if s.events != nil {
			if err := s.events.PublishDisconnected(ctx, account.GetId(), deviceID, isOffline); err != nil {
				logging.Log.Warn().Err(err).Str("accountId", account.GetId()).Str("deviceId", deviceID).Msg("Failed to publish websocket disconnect event")
			}
		}
		_ = conn.Close()
	}()

	for {
		var raw []byte
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			return
		}
		if int64(len(raw)) > s.cfg.MaxMessageBytes {
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "message too large"})
			continue
		}

		var packet Packet
		if err := json.Unmarshal(raw, &packet); err != nil {
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: "unprocessable packet: invalid json"})
			continue
		}

		if err := s.HandlePacket(ctx, account, deviceID, packet); err != nil {
			_ = entry.sendJSON(Packet{Type: PacketTypeError, ErrorMessage: err.Error()})
		}
	}
}

func (c *wsConnection) sendProto(packet *gen.DyWebSocketPacket) error {
	return c.sendJSON(packetFromProto(packet))
}

func (c *wsConnection) sendJSON(packet Packet) error {
	payload, err := json.Marshal(packet)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return websocket.Message.Send(c.conn, payload)
}
