package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Endpoints        EndpointsConfig        `mapstructure:"endpoints"`
	Services         ServicesConfig         `mapstructure:"services"`
	Cache            CacheConfig            `mapstructure:"cache"`
	NATS             NATSConfig             `mapstructure:"nats"`
	RateLimit        RateLimitConfig        `mapstructure:"rateLimit"`
	Health           HealthConfig           `mapstructure:"health"`
	Server           ServerConfig           `mapstructure:"server"`
	GrpcServer       GrpcServerConfig       `mapstructure:"grpcServer"`
	SpecialRoutes    SpecialRoutesConfig    `mapstructure:"specialRoutes"`
	WebSocketGateway WebSocketGatewayConfig `mapstructure:"websocketGateway"`
	SiteURL          string                 `mapstructure:"siteUrl"`
}

type EndpointsConfig struct {
	ServiceNames     []string `mapstructure:"serviceNames"`
	CoreServiceNames []string `mapstructure:"coreServiceNames"`
}

type ServiceEndpoint struct {
	Http string `mapstructure:"http"`
	Grpc string `mapstructure:"grpc"`
}

type ServicesConfig map[string]ServiceEndpoint

type CacheConfig struct {
	Serializer string `mapstructure:"serializer"`
}

type NATSConfig struct {
	URL                    string `mapstructure:"url"`
	WebSocketSubjectPrefix string `mapstructure:"websocketSubjectPrefix"`
}

type RateLimitConfig struct {
	RequestsPerMinute int `mapstructure:"requestsPerMinute"`
	BurstAllowance    int `mapstructure:"burstAllowance"`
}

type HealthConfig struct {
	CheckIntervalSeconds int           `mapstructure:"checkIntervalSeconds"`
	CheckTimeout         time.Duration `mapstructure:"-"`
}

type ServerConfig struct {
	Port         string        `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"readTimeout"`
	WriteTimeout time.Duration `mapstructure:"writeTimeout"`
}

type GrpcServerConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Port    string `mapstructure:"port"`
}

type WebSocketGatewayConfig struct {
	Enabled             bool     `mapstructure:"enabled"`
	Path                string   `mapstructure:"path"`
	AuthService         string   `mapstructure:"authService"`
	AuthUseTLS          bool     `mapstructure:"authUseTLS"`
	AuthTLSSkipVerify   bool     `mapstructure:"authTlsSkipVerify"`
	AuthTLSServerName   string   `mapstructure:"authTlsServerName"`
	KeepAliveSeconds    int      `mapstructure:"keepAliveSeconds"`
	MaxMessageBytes     int64    `mapstructure:"maxMessageBytes"`
	AllowedDeviceAltern []string `mapstructure:"allowedDeviceAlternatives"`
}

type SpecialRoutesConfig struct {
	Routes []RouteRule `mapstructure:"routes"`
}

type RouteRule struct {
	Path    string `mapstructure:"path"`    // source path pattern (e.g., "/ws", "/.well-known/openid-configuration")
	Service string `mapstructure:"service"` // target service name
	Target  string `mapstructure:"target"`  // target path on backend (e.g., "/api/ws", "/auth/.well-known/openid-configuration")
	Prefix  bool   `mapstructure:"prefix"`  // if true, match path prefix (e.g., "/activitypub/**")
}

func Load(configPath string) (*Config, error) {
	viper.SetConfigType("toml")
	viper.SetConfigFile(configPath)

	viper.SetDefault("endpoints.serviceNames", []string{"ring", "pass", "drive", "sphere", "develop", "insight", "zone", "messager"})
	viper.SetDefault("endpoints.coreServiceNames", []string{"ring", "pass", "drive", "sphere"})
	viper.SetDefault("nats.url", "")
	viper.SetDefault("nats.websocketSubjectPrefix", "websocket_")
	viper.SetDefault("rateLimit.requestsPerMinute", 120)
	viper.SetDefault("rateLimit.burstAllowance", 10)
	viper.SetDefault("health.checkIntervalSeconds", 10)
	viper.SetDefault("server.port", "6000")
	viper.SetDefault("server.readTimeout", 60*time.Second)
	viper.SetDefault("server.writeTimeout", 60*time.Second)
	viper.SetDefault("grpcServer.enabled", true)
	viper.SetDefault("grpcServer.port", "7001")
	viper.SetDefault("siteUrl", "http://localhost:3000")

	viper.SetDefault("websocketGateway.enabled", true)
	viper.SetDefault("websocketGateway.path", "/ws")
	viper.SetDefault("websocketGateway.authService", "pass")
	viper.SetDefault("websocketGateway.authUseTLS", false)
	viper.SetDefault("websocketGateway.authTlsSkipVerify", false)
	viper.SetDefault("websocketGateway.authTlsServerName", "")
	viper.SetDefault("websocketGateway.keepAliveSeconds", 60)
	viper.SetDefault("websocketGateway.maxMessageBytes", 4096)
	viper.SetDefault("websocketGateway.allowedDeviceAlternatives", []string{"watch"})

	viper.SetDefault("specialRoutes.routes", []RouteRule{
		{Path: "/.well-known/openid-configuration", Service: "pass", Target: "/auth/.well-known/openid-configuration", Prefix: false},
		{Path: "/.well-known/jwks", Service: "pass", Target: "/auth/.well-known/jwks", Prefix: false},
		{Path: "/.well-known/webfinger", Service: "sphere", Target: "/fediverse/.well-known/webfinger", Prefix: false},
		{Path: "/activitypub", Service: "sphere", Target: "/activitypub", Prefix: true},
		{Path: "/api/activitypub", Service: "sphere", Target: "/activitypub", Prefix: true},
	})

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	cfg.Health.CheckTimeout = 5 * time.Second

	return &cfg, nil
}

func GetServiceHttp(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.http", serviceName))
}

func GetServiceGrpc(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.grpc", serviceName))
}
