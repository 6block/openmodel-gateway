package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode    string        `yaml:"mode"` // "dev" or "prod"
	Gateway GatewayConfig `yaml:"gateway"`
	Workers WorkerConfig  `yaml:"workers"`
	Admin   AdminConfig   `yaml:"admin"`
	Logging LoggingConfig `yaml:"logging"`
	Metrics MetricsConfig `yaml:"metrics"`
}

type GatewayConfig struct {
	Port              int        `yaml:"port"`
	ClientToken       string     `yaml:"client_token"`        // Single token (backward compat); empty = auth disabled
	APIKeys           []APIKey   `yaml:"api_keys"`            // Multi-key support (takes precedence over client_token)
	RequestTimeoutSec      int        `yaml:"request_timeout_sec"`       // Timeout for proxied requests to workers
	QueueTimeoutSec        int        `yaml:"queue_timeout_sec"`         // Max seconds to wait when all workers are mining
	MaxQueueSize           int        `yaml:"max_queue_size"`            // Max queued requests; 0 = unlimited
	ModelSwitchLoadFactor  float64    `yaml:"model_switch_load_factor"`  // Trigger model switch when active_requests > gpu_count * this; 0 = disabled
	RequestLogPath         string     `yaml:"request_log_path"`          // Path to persist request records; empty = no persistence
}

// APIKey represents a client API key with metadata for billing (M3 prep).
type APIKey struct {
	Key    string `yaml:"key"`
	Name   string `yaml:"name"`              // Human-readable label
	Wallet string `yaml:"wallet,omitempty"`   // Filecoin wallet address (M3)
}

type WorkerConfig struct {
	PollIntervalSec      int `yaml:"poll_interval_sec"`
	OfflineTimeoutSec    int `yaml:"offline_timeout_sec"`
	OfflineFailThreshold int `yaml:"offline_fail_threshold"` // Consecutive poll failures before offline
}

type AdminConfig struct {
	Port  int    `yaml:"port"`
	Token string `yaml:"token"` // Bearer token for /api/v1/* endpoints; empty = auth disabled
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Load reads and parses the config file, interpolating environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	content := interpolateEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks for required fields based on mode.
func validate(cfg *Config) error {
	if cfg.Workers.PollIntervalSec < 1 {
		return fmt.Errorf("workers.poll_interval_sec must be >= 1")
	}
	if cfg.Workers.OfflineFailThreshold < 1 {
		return fmt.Errorf("workers.offline_fail_threshold must be >= 1")
	}
	return nil
}

func interpolateEnvVars(content string) string {
	result := content
	for {
		start := strings.Index(result, "${")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "}")
		if end == -1 {
			break
		}
		end += start

		envExpr := result[start+2 : end]
		var envVal string
		// Support ${VAR:-default} syntax
		if idx := strings.Index(envExpr, ":-"); idx != -1 {
			envName := envExpr[:idx]
			defaultVal := envExpr[idx+2:]
			envVal = os.Getenv(envName)
			if envVal == "" {
				envVal = defaultVal
			}
		} else {
			envVal = os.Getenv(envExpr)
		}
		result = result[:start] + envVal + result[end+1:]
	}
	return result
}

func applyDefaults(cfg *Config) {
	if cfg.Mode == "" {
		cfg.Mode = "dev"
	}
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = 3000
	}
	if cfg.Gateway.RequestTimeoutSec == 0 {
		cfg.Gateway.RequestTimeoutSec = 120
	}
	if cfg.Gateway.QueueTimeoutSec == 0 {
		cfg.Gateway.QueueTimeoutSec = 60
	}
	if cfg.Workers.PollIntervalSec == 0 {
		cfg.Workers.PollIntervalSec = 5
	}
	if cfg.Workers.OfflineTimeoutSec == 0 {
		cfg.Workers.OfflineTimeoutSec = 30
	}
	if cfg.Workers.OfflineFailThreshold == 0 {
		cfg.Workers.OfflineFailThreshold = 3
	}
	if cfg.Admin.Port == 0 {
		cfg.Admin.Port = 9091
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
}
