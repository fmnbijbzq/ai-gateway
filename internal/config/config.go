package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server         ServerConfig         `yaml:"server"`
	HealthCheck    HealthCheckConfig    `yaml:"health_check"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	Upstreams      []UpstreamConfig     `yaml:"upstreams"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	MaxRetries   int           `yaml:"max_retries"`
	APIKey       string        `yaml:"api_key"`
}

type HealthCheckConfig struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailureThreshold int           `yaml:"failure_threshold"`
	RecoveryCooldown time.Duration `yaml:"recovery_cooldown"`
}

type CircuitBreakerConfig struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	RecoveryTimeout  time.Duration `yaml:"recovery_timeout"`
}

type UpstreamConfig struct {
	Name    string   `yaml:"name"`
	BaseURL string   `yaml:"base_url"`
	APIKey  string   `yaml:"api_key"`
	Weight  int      `yaml:"weight"`
	Models  []string `yaml:"models"`
	APIType string   `yaml:"api_type"` // "openai" (default) or "anthropic"
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 30 * time.Second
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 120 * time.Second
	}
	if c.Server.MaxRetries == 0 {
		c.Server.MaxRetries = 3
	}
	if c.HealthCheck.Interval == 0 {
		c.HealthCheck.Interval = 10 * time.Second
	}
	if c.HealthCheck.Timeout == 0 {
		c.HealthCheck.Timeout = 5 * time.Second
	}
	if c.HealthCheck.FailureThreshold == 0 {
		c.HealthCheck.FailureThreshold = 3
	}
	if c.HealthCheck.RecoveryCooldown == 0 {
		c.HealthCheck.RecoveryCooldown = 30 * time.Second
	}
	if c.CircuitBreaker.FailureThreshold == 0 {
		c.CircuitBreaker.FailureThreshold = 5
	}
	if c.CircuitBreaker.RecoveryTimeout == 0 {
		c.CircuitBreaker.RecoveryTimeout = 30 * time.Second
	}

	for i := range c.Upstreams {
		if c.Upstreams[i].Weight == 0 {
			c.Upstreams[i].Weight = 1
		}
	}
}

func (c *Config) validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	seen := make(map[string]bool)
	for i, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstream[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = true
		if u.BaseURL == "" {
			return fmt.Errorf("upstream %q: base_url is required", u.Name)
		}
		if u.APIKey == "" {
			return fmt.Errorf("upstream %q: api_key is required", u.Name)
		}
		if u.Weight < 0 {
			return fmt.Errorf("upstream %q: weight must be non-negative", u.Name)
		}
	}
	return nil
}
