package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Endpoints EndpointsConfig `mapstructure:"endpoints"`
	Services  ServicesConfig  `mapstructure:"services"`
	Cache     CacheConfig     `mapstructure:"cache"`
	RateLimit RateLimitConfig `mapstructure:"rateLimit"`
	Health    HealthConfig    `mapstructure:"health"`
	Server    ServerConfig    `mapstructure:"server"`
	SiteURL   string          `mapstructure:"siteUrl"`
}

type EndpointsConfig struct {
	ServiceNames     []string `mapstructure:"serviceNames"`
	CoreServiceNames []string `mapstructure:"coreServiceNames"`
}

type ServiceEndpoint struct {
	HTTP string `mapstructure:"http"`
	GRPC string `mapstructure:"grpc"`
}

type ServicesConfig map[string]ServiceEndpoint

type CacheConfig struct {
	Serializer string `mapstructure:"serializer"`
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

func Load(configPath string) (*Config, error) {
	viper.SetConfigType("toml")
	viper.SetConfigFile(configPath)

	viper.SetDefault("endpoints.serviceNames", []string{"ring", "pass", "drive", "sphere", "develop", "insight", "zone", "messager"})
	viper.SetDefault("endpoints.coreServiceNames", []string{"ring", "pass", "drive", "sphere"})
	viper.SetDefault("rateLimit.requestsPerMinute", 120)
	viper.SetDefault("rateLimit.burstAllowance", 10)
	viper.SetDefault("health.checkIntervalSeconds", 10)
	viper.SetDefault("server.port", "6000")
	viper.SetDefault("server.readTimeout", 60*time.Second)
	viper.SetDefault("server.writeTimeout", 60*time.Second)
	viper.SetDefault("siteUrl", "http://localhost:3000")

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

func GetServiceHTTP(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.http", serviceName))
}

func GetServiceGRPC(serviceName string) string {
	return viper.GetString(fmt.Sprintf("services.%s.grpc", serviceName))
}
