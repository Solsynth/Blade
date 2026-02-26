package config

import "testing"

func TestLoadConfig_SpecialRoutesIncludesWS(t *testing.T) {
	cfg, err := Load("../../configs/config.toml")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if len(cfg.SpecialRoutes.Routes) == 0 {
		t.Fatalf("expected special routes to be loaded, got 0")
	}

	foundWS := false
	for _, r := range cfg.SpecialRoutes.Routes {
		if r.Path == "/ws" {
			foundWS = true
			break
		}
	}
	if foundWS {
		t.Fatalf("expected /ws to be handled by websocketGateway, not specialRoutes; got: %+v", cfg.SpecialRoutes.Routes)
	}

	if !cfg.WebSocketGateway.Enabled {
		t.Fatal("expected websocketGateway.enabled=true")
	}
	if cfg.WebSocketGateway.Path != "/ws" {
		t.Fatalf("expected websocketGateway.path=/ws, got %q", cfg.WebSocketGateway.Path)
	}
	if cfg.WebSocketGateway.AuthService != "pass" {
		t.Fatalf("expected websocketGateway.authService=pass, got %q", cfg.WebSocketGateway.AuthService)
	}
}
