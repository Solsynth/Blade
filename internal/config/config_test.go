package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_SpecialRoutesIncludesWS(t *testing.T) {
	cfg, err := Load("../../configs/config.toml")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if len(cfg.Routes) == 0 {
		t.Fatalf("expected routes to be loaded, got 0")
	}

	foundWS := false
	for _, r := range cfg.Routes {
		if r.Path == "/ws" {
			foundWS = true
			break
		}
	}
	if foundWS {
		t.Fatalf("expected /ws to be handled by websocket, not routes; got: %+v", cfg.Routes)
	}

	if !cfg.WebSocket.Enabled {
		t.Fatal("expected websocket.enabled=true")
	}
	if cfg.WebSocket.Path != "/ws" {
		t.Fatalf("expected websocket.path=/ws, got %q", cfg.WebSocket.Path)
	}
	if cfg.WebSocket.AuthService != "padlock" {
		t.Fatalf("expected websocket.authService=padlock, got %q", cfg.WebSocket.AuthService)
	}
}

func TestLoadConfig_LegacyKeysStillSupported(t *testing.T) {
	toml := `
[grpcServer]
enabled = true
port = "7001"

[websocketGateway]
enabled = true
path = "/ws"
authService = "pass"
maxMessageBytes = 1234

[[specialRoutes.routes]]
path = "/legacy"
service = "pass"
target = "/auth/legacy"
prefix = false

[maintaince]
enabled = true
mode = "service"
services = ["sphere"]
`
	tmpPath := filepath.Join(t.TempDir(), "legacy.toml")
	if err := os.WriteFile(tmpPath, []byte(toml), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(tmpPath)
	if err != nil {
		t.Fatalf("failed to load legacy config: %v", err)
	}

	if !cfg.GRPC.Enabled || cfg.GRPC.Port != "7001" {
		t.Fatalf("expected grpc from legacy key, got %+v", cfg.GRPC)
	}
	if !cfg.WebSocket.Enabled || cfg.WebSocket.Path != "/ws" || cfg.WebSocket.MaxMessageBytes != 1234 {
		t.Fatalf("expected websocket from legacy key, got %+v", cfg.WebSocket)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Path != "/legacy" {
		t.Fatalf("expected routes from legacy key, got %+v", cfg.Routes)
	}
	if !cfg.Maintenance.Enabled || cfg.Maintenance.Mode != "service" || len(cfg.Maintenance.Services) != 1 || cfg.Maintenance.Services[0] != "sphere" {
		t.Fatalf("expected maintenance from legacy key, got %+v", cfg.Maintenance)
	}
}

func TestLoadConfig_MaintenanceMode(t *testing.T) {
	toml := `
[maintenance]
enabled = true
mode = "full"
services = ["sphere", "pass"]
`

	tmpPath := filepath.Join(t.TempDir(), "maintenance.toml")
	if err := os.WriteFile(tmpPath, []byte(toml), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(tmpPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if !cfg.Maintenance.Enabled {
		t.Fatal("expected maintenance.enabled=true")
	}
	if cfg.Maintenance.Mode != "full" {
		t.Fatalf("expected maintenance.mode=full, got %q", cfg.Maintenance.Mode)
	}
	if len(cfg.Maintenance.Services) != 2 {
		t.Fatalf("expected 2 maintenance services, got %d", len(cfg.Maintenance.Services))
	}
}
