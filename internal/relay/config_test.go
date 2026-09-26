package relay

import (
	"os"
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	cfg := Config{
		Relay: RelayConfig{
			Listen:              ":7443",
			PublicHost:          "relay-jp.solian.app",
			PublicPort:          443,
			ID:                  "jp-01",
			Weight:              1,
			DialTimeout:         5 * time.Second,
			SNITimeout:          5 * time.Second,
			MaxClientHelloBytes: 8192,
			Upstreams:           []UpstreamRule{{SNI: "api.solian.app", Target: "api.solian.app:443"}},
		},
		Health: HealthConfig{Listen: ":7481"},
		Discovery: DiscoveryConfig{
			URL:               "https://api.solian.app",
			RegistrationToken: "secret",
		},
	}
	cfg.Normalize()
	return &cfg
}

func TestValidateRelayConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(*Config){
		"missing listen":           func(c *Config) { c.Relay.Listen = "" },
		"public port out of range": func(c *Config) { c.Relay.PublicPort = 70000 },
		"zero dial timeout":        func(c *Config) { c.Relay.DialTimeout = 0 },
		"zero sni timeout":         func(c *Config) { c.Relay.SNITimeout = 0 },
		"zero max client hello":    func(c *Config) { c.Relay.MaxClientHelloBytes = 0 },
		"negative max connections": func(c *Config) { c.Relay.MaxConnections = -1 },
		"zero weight":              func(c *Config) { c.Relay.Weight = 0 },
		"no upstreams and no default": func(c *Config) {
			c.Relay.Upstreams = nil
			c.Relay.DefaultUpstream = ""
		},
		"upstream without sni":  func(c *Config) { c.Relay.Upstreams[0].SNI = "" },
		"upstream without port": func(c *Config) { c.Relay.Upstreams[0].Target = "api.solian.app" },
		"upstream empty target": func(c *Config) { c.Relay.Upstreams[0].Target = "" },
		"bad default upstream":  func(c *Config) { c.Relay.DefaultUpstream = "api.solian.app" },
		"discovery without url": func(c *Config) {
			c.Discovery.Enabled = true
			c.Discovery.URL = ""
		},
		"discovery with relative url": func(c *Config) {
			c.Discovery.Enabled = true
			c.Discovery.URL = "api.solian.app"
		},
		"discovery without token": func(c *Config) {
			c.Discovery.Enabled = true
			c.Discovery.RegistrationToken = ""
		},
		"discovery without public host": func(c *Config) {
			c.Discovery.Enabled = true
			c.Relay.PublicHost = ""
		},
	}

	for name, mutate := range cases {
		cfg := validConfig()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestValidateRejectsDuplicateSNI(t *testing.T) {
	cfg := validConfig()
	cfg.Relay.Upstreams = append(cfg.Relay.Upstreams, UpstreamRule{
		SNI:    "API.Solian.App.", // same rule modulo case and trailing dot
		Target: "api.solian.app:443",
	})
	cfg.Normalize()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Validate() = %v, want a duplicate sni error", err)
	}
}

func TestNormalizeFillsIdentityAndCanonicalizesSNI(t *testing.T) {
	cfg := Config{
		Relay: RelayConfig{
			ID:        "  ",
			Region:    "  JP ",
			Listen:    " :7443 ",
			Upstreams: []UpstreamRule{{SNI: " API.Solian.App. ", Target: " api.solian.app:443 "}},
		},
	}
	cfg.Normalize()

	hostname, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	if cfg.Relay.ID != strings.TrimSpace(hostname) {
		t.Fatalf("Relay.ID = %q, want the hostname %q", cfg.Relay.ID, hostname)
	}
	if cfg.Relay.Region != "jp" || cfg.Relay.Listen != ":7443" {
		t.Fatalf("Relay = %+v, want trimmed listen and lowercased region", cfg.Relay)
	}
	if cfg.Relay.Upstreams[0].SNI != "api.solian.app" {
		t.Fatalf("Upstreams[0].SNI = %q, want a canonical SNI", cfg.Relay.Upstreams[0].SNI)
	}
	if cfg.Relay.Upstreams[0].Target != "api.solian.app:443" {
		t.Fatalf("Upstreams[0].Target = %q, want a trimmed target", cfg.Relay.Upstreams[0].Target)
	}

	// Normalize is idempotent.
	once := cfg
	cfg.Normalize()
	if cfg.Relay.Upstreams[0] != once.Relay.Upstreams[0] || cfg.Relay.Region != once.Relay.Region {
		t.Fatal("Normalize must be idempotent")
	}
}

func TestPublicAddress(t *testing.T) {
	cfg := validConfig()
	if got := cfg.Relay.PublicAddress(); got != "relay-jp.solian.app:443" {
		t.Fatalf("PublicAddress() = %q, want the dialed public address", got)
	}
}

func TestLoadSampleConfig(t *testing.T) {
	cfg, err := Load("../../configs/relay.toml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Relay.DialTimeout != 5*time.Second || cfg.Relay.SNITimeout != 5*time.Second {
		t.Fatalf("durations = %v/%v, want 5s", cfg.Relay.DialTimeout, cfg.Relay.SNITimeout)
	}
	if cfg.Relay.IdleTimeout != 0 || cfg.Relay.MaxConnections != 0 {
		t.Fatalf("bounds = %v/%d, want unlimited by default", cfg.Relay.IdleTimeout, cfg.Relay.MaxConnections)
	}
	if len(cfg.Relay.Upstreams) != 2 || cfg.Relay.Upstreams[0].SNI != "api.solian.app" {
		t.Fatalf("upstreams = %+v", cfg.Relay.Upstreams)
	}
	if !cfg.Discovery.Enabled || cfg.Discovery.URL != "https://api.solian.app" {
		t.Fatalf("discovery = %+v", cfg.Discovery)
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.toml"); err == nil {
		t.Fatal("Load() must fail when the config file is missing")
	}
}
