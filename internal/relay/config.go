// Package relay implements Blade's L4 relay node. A relay accepts TCP
// connections, peeks the cleartext TLS ClientHello for the requested SNI,
// picks an upstream from a static allowlist, and copies bytes verbatim in both
// directions. TLS stays end-to-end: the relay never terminates it, never sees
// plaintext, and replays the bytes it read while peeking.
package relay

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	defaultListenAddr            = ":443"
	defaultHealthListenAddr      = ":8081"
	defaultSNITimeout            = 5 * time.Second
	defaultDialTimeout           = 5 * time.Second
	defaultMaxClientHelloBytes   = 8192
	defaultDiscoveryService      = "relay"
	defaultDiscoveryLeaseSeconds = 30
)

// Config is the full relay configuration, loaded from a single TOML file.
type Config struct {
	Relay     RelayConfig     `mapstructure:"relay"`
	Health    HealthConfig    `mapstructure:"health"`
	Discovery DiscoveryConfig `mapstructure:"discovery"`
	Log       LogConfig       `mapstructure:"log"`
}

// RelayConfig holds the listener, the advertised public address, the resource
// bounds, and the SNI allowlist.
type RelayConfig struct {
	Listen              string         `mapstructure:"listen"`
	PublicHost          string         `mapstructure:"publicHost"`
	PublicPort          int            `mapstructure:"publicPort"`
	ID                  string         `mapstructure:"id"`
	Region              string         `mapstructure:"region"`
	Weight              int32          `mapstructure:"weight"`
	MaxConnections      int64          `mapstructure:"maxConnections"`
	DialTimeout         time.Duration  `mapstructure:"dialTimeout"`
	IdleTimeout         time.Duration  `mapstructure:"idleTimeout"`
	SNITimeout          time.Duration  `mapstructure:"sniTimeout"`
	MaxClientHelloBytes int            `mapstructure:"maxClientHelloBytes"`
	DefaultUpstream     string         `mapstructure:"defaultUpstream"`
	Upstreams           []UpstreamRule `mapstructure:"upstreams"`
}

// UpstreamRule maps one SNI to one host:port origin. The target must carry an
// explicit port; there is no implicit 443.
type UpstreamRule struct {
	SNI    string `mapstructure:"sni"`
	Target string `mapstructure:"target"`
}

// HealthConfig configures the relay's own HTTP status listener, which Blade
// probes for liveness.
type HealthConfig struct {
	Listen    string `mapstructure:"listen"`
	Advertise string `mapstructure:"advertise"`
}

// DiscoveryConfig configures the registration of this relay in Blade's
// Redis-backed service registry.
type DiscoveryConfig struct {
	Enabled           bool   `mapstructure:"enabled"`
	Target            string `mapstructure:"target"`
	UseTLS            bool   `mapstructure:"useTLS"`
	TLSSkipVerify     bool   `mapstructure:"tlsSkipVerify"`
	TLSServerName     string `mapstructure:"tlsServerName"`
	RegistrationToken string `mapstructure:"registrationToken"`
	Service           string `mapstructure:"service"`
	LeaseSeconds      int    `mapstructure:"leaseSeconds"`
}

// LogConfig selects the log output format.
type LogConfig struct {
	Pretty bool `mapstructure:"pretty"`
}

// Load reads and validates the relay configuration at path.
func Load(path string) (*Config, error) {
	viper.Reset()
	viper.SetConfigType("toml")
	viper.SetConfigFile(path)

	viper.SetDefault("relay.listen", defaultListenAddr)
	viper.SetDefault("relay.publicHost", "")
	viper.SetDefault("relay.publicPort", 443)
	viper.SetDefault("relay.id", "")
	viper.SetDefault("relay.region", "")
	viper.SetDefault("relay.weight", 1)
	viper.SetDefault("relay.maxConnections", 0)
	viper.SetDefault("relay.dialTimeout", defaultDialTimeout)
	viper.SetDefault("relay.idleTimeout", time.Duration(0))
	viper.SetDefault("relay.sniTimeout", defaultSNITimeout)
	viper.SetDefault("relay.maxClientHelloBytes", defaultMaxClientHelloBytes)
	viper.SetDefault("relay.defaultUpstream", "")
	viper.SetDefault("relay.upstreams", []UpstreamRule{})

	viper.SetDefault("health.listen", defaultHealthListenAddr)
	viper.SetDefault("health.advertise", "")

	viper.SetDefault("discovery.enabled", false)
	viper.SetDefault("discovery.target", "")
	viper.SetDefault("discovery.useTLS", false)
	viper.SetDefault("discovery.tlsSkipVerify", false)
	viper.SetDefault("discovery.tlsServerName", "")
	viper.SetDefault("discovery.registrationToken", "")
	viper.SetDefault("discovery.service", defaultDiscoveryService)
	viper.SetDefault("discovery.leaseSeconds", defaultDiscoveryLeaseSeconds)

	viper.SetDefault("log.pretty", false)

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read relay config: %w", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relay config: %w", err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Normalize canonicalizes every field that is matched or dialed later: SNI
// keys are lowercased with trailing dots stripped, addresses are trimmed, and
// a missing relay id falls back to the hostname. It is idempotent.
func (c *Config) Normalize() {
	c.Relay.Listen = strings.TrimSpace(c.Relay.Listen)
	c.Relay.PublicHost = strings.TrimSpace(c.Relay.PublicHost)
	c.Relay.Region = strings.ToLower(strings.TrimSpace(c.Relay.Region))
	c.Relay.DefaultUpstream = strings.TrimSpace(c.Relay.DefaultUpstream)
	if c.Relay.ID = strings.TrimSpace(c.Relay.ID); c.Relay.ID == "" {
		if host, err := os.Hostname(); err == nil {
			c.Relay.ID = strings.TrimSpace(host)
		}
	}
	for i := range c.Relay.Upstreams {
		c.Relay.Upstreams[i].SNI = NormalizeSNI(c.Relay.Upstreams[i].SNI)
		c.Relay.Upstreams[i].Target = strings.TrimSpace(c.Relay.Upstreams[i].Target)
	}

	c.Health.Listen = strings.TrimSpace(c.Health.Listen)
	c.Health.Advertise = strings.TrimSpace(c.Health.Advertise)

	c.Discovery.Target = strings.TrimSpace(c.Discovery.Target)
	c.Discovery.TLSServerName = strings.TrimSpace(c.Discovery.TLSServerName)
	c.Discovery.Service = strings.ToLower(strings.TrimSpace(c.Discovery.Service))
}

// NormalizeSNI is the single SNI comparison rule: case-insensitive, trailing
// dots ignored. Both the allowlist and the peeked ClientHello go through it.
func NormalizeSNI(sni string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(sni), "."))
}

// Validate returns the first configuration error, or nil. Load calls it, so a
// loaded Config is always usable.
func (c *Config) Validate() error {
	if c.Relay.Listen == "" {
		return fmt.Errorf("relay.listen is required")
	}
	if c.Relay.PublicPort < 1 || c.Relay.PublicPort > 65535 {
		return fmt.Errorf("relay.publicPort must be between 1 and 65535, got %d", c.Relay.PublicPort)
	}
	if c.Relay.DialTimeout <= 0 {
		return fmt.Errorf("relay.dialTimeout must be positive")
	}
	if c.Relay.SNITimeout <= 0 {
		return fmt.Errorf("relay.sniTimeout must be positive")
	}
	if c.Relay.MaxClientHelloBytes <= 0 {
		return fmt.Errorf("relay.maxClientHelloBytes must be positive")
	}
	if c.Relay.MaxConnections < 0 {
		return fmt.Errorf("relay.maxConnections must not be negative")
	}
	if c.Relay.Weight < 1 {
		return fmt.Errorf("relay.weight must be greater than zero")
	}
	if len(c.Relay.Upstreams) == 0 && c.Relay.DefaultUpstream == "" {
		return fmt.Errorf("at least one relay.upstreams rule or relay.defaultUpstream is required")
	}
	if c.Relay.DefaultUpstream != "" {
		if err := validateTarget("relay.defaultUpstream", c.Relay.DefaultUpstream); err != nil {
			return err
		}
	}

	seen := make(map[string]struct{}, len(c.Relay.Upstreams))
	for _, rule := range c.Relay.Upstreams {
		if rule.SNI == "" {
			return fmt.Errorf("relay.upstreams entry is missing sni")
		}
		if err := validateTarget("relay.upstreams."+rule.SNI, rule.Target); err != nil {
			return err
		}
		if _, duplicate := seen[rule.SNI]; duplicate {
			return fmt.Errorf("relay.upstreams contains duplicate sni %q", rule.SNI)
		}
		seen[rule.SNI] = struct{}{}
	}

	if c.Discovery.Enabled {
		if c.Discovery.Target == "" {
			return fmt.Errorf("discovery.target is required when discovery is enabled")
		}
		if c.Discovery.RegistrationToken == "" {
			return fmt.Errorf("discovery.registrationToken is required when discovery is enabled")
		}
		if c.Discovery.Service == "" {
			return fmt.Errorf("discovery.service is required when discovery is enabled")
		}
		if c.Discovery.LeaseSeconds < 3 {
			return fmt.Errorf("discovery.leaseSeconds must be at least three seconds")
		}
		if c.Relay.PublicHost == "" {
			return fmt.Errorf("relay.publicHost is required when discovery is enabled")
		}
		if c.Relay.ID == "" {
			return fmt.Errorf("relay.id is required when discovery is enabled and the hostname is unavailable")
		}
	}
	return nil
}

func validateTarget(field, target string) error {
	if target == "" {
		return fmt.Errorf("%s is required", field)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("%s must be host:port: %w", field, err)
	}
	if host == "" {
		return fmt.Errorf("%s must include a host", field)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("%s must include a port between 1 and 65535", field)
	}
	return nil
}
