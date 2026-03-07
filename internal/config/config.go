package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Endpoints   EndpointsConfig   `mapstructure:"endpoints"`
	Services    ServicesConfig    `mapstructure:"services"`
	Cache       CacheConfig       `mapstructure:"cache"`
	NATS        NatsConfig        `mapstructure:"nats"`
	Health      HealthConfig      `mapstructure:"health"`
	Server      ServerConfig      `mapstructure:"server"`
	GRPC        GrpcServerConfig  `mapstructure:"grpc"`
	WebSocket   WebSocketConfig   `mapstructure:"websocket"`
	Routes      []RouteRule       `mapstructure:"routes"`
	Maintenance MaintenanceConfig `mapstructure:"maintenance"`
	SiteURL     string            `mapstructure:"siteUrl"`
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

type NatsConfig struct {
	URL                    string `mapstructure:"url"`
	WebSocketSubjectPrefix string `mapstructure:"websocketSubjectPrefix"`
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

type WebSocketConfig struct {
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

type RouteRule struct {
	Path    string `mapstructure:"path"`    // source path pattern (e.g., "/ws", "/.well-known/openid-configuration")
	Service string `mapstructure:"service"` // target service name
	Target  string `mapstructure:"target"`  // target path on backend (e.g., "/api/ws", "/auth/.well-known/openid-configuration")
	Prefix  bool   `mapstructure:"prefix"`  // if true, match path prefix (e.g., "/activitypub/**")
}

type MaintenanceConfig struct {
	Enabled  bool     `mapstructure:"enabled"`
	Mode     string   `mapstructure:"mode"`
	Services []string `mapstructure:"services"`
}

func Load(configPath string) (*Config, error) {
	viper.Reset()
	viper.SetConfigType("toml")
	viper.SetConfigFile(configPath)

	viper.SetDefault("endpoints.serviceNames", []string{"ring", "pass", "drive", "sphere", "develop", "insight", "zone", "messager"})
	viper.SetDefault("endpoints.coreServiceNames", []string{"ring", "pass", "drive", "sphere"})
	viper.SetDefault("nats.url", "")
	viper.SetDefault("nats.websocketSubjectPrefix", "websocket_")
	viper.SetDefault("health.checkIntervalSeconds", 10)
	viper.SetDefault("server.port", "6000")
	viper.SetDefault("server.readTimeout", 60*time.Second)
	viper.SetDefault("server.writeTimeout", 60*time.Second)
	viper.SetDefault("grpc.enabled", true)
	viper.SetDefault("grpc.port", "7001")
	viper.SetDefault("siteUrl", "http://localhost:3000")

	viper.SetDefault("websocket.enabled", true)
	viper.SetDefault("websocket.path", "/ws")
	viper.SetDefault("websocket.authService", "pass")
	viper.SetDefault("websocket.authUseTLS", false)
	viper.SetDefault("websocket.authTlsSkipVerify", false)
	viper.SetDefault("websocket.authTlsServerName", "")
	viper.SetDefault("websocket.keepAliveSeconds", 60)
	viper.SetDefault("websocket.maxMessageBytes", 4096)
	viper.SetDefault("websocket.allowedDeviceAlternatives", []string{"watch"})
	viper.SetDefault("maintenance.enabled", false)
	viper.SetDefault("maintenance.mode", "full")
	viper.SetDefault("maintenance.services", []string{})

	viper.SetDefault("routes", []RouteRule{
		{Path: "/.well-known/openid-configuration", Service: "pass", Target: "/auth/.well-known/openid-configuration", Prefix: false},
		{Path: "/.well-known/jwks", Service: "pass", Target: "/auth/.well-known/jwks", Prefix: false},
		{Path: "/.well-known/webfinger", Service: "sphere", Target: "/fediverse/.well-known/webfinger", Prefix: false},
		{Path: "/activitypub", Service: "sphere", Target: "/activitypub", Prefix: true},
		{Path: "/api/activitypub", Service: "sphere", Target: "/activitypub", Prefix: true},
	})

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	applyLegacyAliases()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	cfg.Health.CheckTimeout = 5 * time.Second

	return &cfg, nil
}

func applyLegacyAliases() {
	if !hasNewGRPCConfig() && hasLegacyGRPCConfig() {
		viper.Set("grpc", viper.Get("grpcServer"))
	}
	if !hasNewWebSocketConfig() && hasLegacyWebSocketConfig() {
		viper.Set("websocket", viper.Get("websocketGateway"))
	}
	if !hasNewRoutesConfig() && hasLegacyRoutesConfig() {
		viper.Set("routes", viper.Get("specialRoutes.routes"))
	}
	if !hasNewMaintenanceConfig() && hasLegacyMaintenanceConfig() {
		viper.Set("maintenance", viper.Get("maintaince"))
	}
}

func hasNewGRPCConfig() bool {
	return viper.InConfig("grpc.enabled") || viper.InConfig("grpc.port")
}

func hasLegacyGRPCConfig() bool {
	return viper.InConfig("grpcServer.enabled") || viper.InConfig("grpcServer.port")
}

func hasNewWebSocketConfig() bool {
	return viper.InConfig("websocket.enabled") ||
		viper.InConfig("websocket.path") ||
		viper.InConfig("websocket.authService") ||
		viper.InConfig("websocket.authUseTLS") ||
		viper.InConfig("websocket.authTlsSkipVerify") ||
		viper.InConfig("websocket.authTlsServerName") ||
		viper.InConfig("websocket.keepAliveSeconds") ||
		viper.InConfig("websocket.maxMessageBytes") ||
		viper.InConfig("websocket.allowedDeviceAlternatives")
}

func hasLegacyWebSocketConfig() bool {
	return viper.InConfig("websocketGateway.enabled") ||
		viper.InConfig("websocketGateway.path") ||
		viper.InConfig("websocketGateway.authService") ||
		viper.InConfig("websocketGateway.authUseTLS") ||
		viper.InConfig("websocketGateway.authTlsSkipVerify") ||
		viper.InConfig("websocketGateway.authTlsServerName") ||
		viper.InConfig("websocketGateway.keepAliveSeconds") ||
		viper.InConfig("websocketGateway.maxMessageBytes") ||
		viper.InConfig("websocketGateway.allowedDeviceAlternatives")
}

func hasNewRoutesConfig() bool {
	return viper.InConfig("routes")
}

func hasLegacyRoutesConfig() bool {
	return viper.InConfig("specialRoutes.routes")
}

func hasNewMaintenanceConfig() bool {
	return viper.InConfig("maintenance.enabled") ||
		viper.InConfig("maintenance.mode") ||
		viper.InConfig("maintenance.services")
}

func hasLegacyMaintenanceConfig() bool {
	return viper.InConfig("maintaince.enabled") ||
		viper.InConfig("maintaince.mode") ||
		viper.InConfig("maintaince.services")
}

func GetServiceHttp(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.http", serviceName))
}

func GetServiceGrpc(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.grpc", serviceName))
}
