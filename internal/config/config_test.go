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
		if r.Path == "/ws" && r.Service == "ring" && r.Target == "/api/ws" && !r.Prefix {
			foundWS = true
			break
		}
	}
	if !foundWS {
		t.Fatalf("expected /ws special route (ring -> /api/ws) to be present, got: %+v", cfg.SpecialRoutes.Routes)
	}
}

