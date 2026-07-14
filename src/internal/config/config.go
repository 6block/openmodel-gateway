package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"openmodel/sp-state-agent/internal/settlement"
)

type Config struct {
	Mode         string             `yaml:"mode"` // "dev" or "prod"
	Gateway      GatewayConfig      `yaml:"gateway"`
	Workers      WorkerConfig       `yaml:"workers"`
	Admin        AdminConfig        `yaml:"admin"`
	PublicQuery  PublicQueryConfig  `yaml:"public_query"`
	Verification VerificationConfig `yaml:"verification"`
	Logging      LoggingConfig      `yaml:"logging"`
	Metrics      MetricsConfig      `yaml:"metrics"`
	Settlement   settlement.Config  `yaml:"settlement"`
}

// VerificationConfig controls sampling + retention of request/response pairs for SP
// fraud detection (byzantine SP: model substitution, token/composition misreporting).
// It retains a random fraction of served requests (prompt + response + claimed model +
// reported tokens) so they can be re-checked offline against a reference — capability
// probes, re-inference comparison, cache/composition anomaly analysis. Disabled by
// default (opt-in) — it stores user content, so mind privacy before enabling in prod.
type VerificationConfig struct {
	SampleRate    float64 `yaml:"sample_rate"`     // fraction of served requests to retain (0 = off)
	SampleLogPath string  `yaml:"sample_log_path"` // JSONL path; empty = off even if rate > 0
	SampleMaxMB   int     `yaml:"sample_max_mb"`   // rotation size per file (MB); 0 = default 50
	SampleBackups int     `yaml:"sample_backups"`  // numbered backups kept; 0 = default 5
}

type GatewayConfig struct {
	Port                  int      `yaml:"port"`
	ClientToken           string   `yaml:"client_token"`             // Single token (backward compat); empty = auth disabled
	APIKeys               []APIKey `yaml:"api_keys"`                 // Multi-key support (takes precedence over client_token)
	RequestTimeoutSec     int      `yaml:"request_timeout_sec"`      // Timeout for proxied requests to workers
	QueueTimeoutSec       int      `yaml:"queue_timeout_sec"`        // Max seconds to wait when all workers are mining
	MaxQueueSize          int      `yaml:"max_queue_size"`           // Max queued requests; 0 = unlimited
	ModelSwitchLoadFactor float64  `yaml:"model_switch_load_factor"` // Trigger model switch when active_requests > gpu_count * this; 0 = disabled
	RequestLogPath        string   `yaml:"request_log_path"`         // Path to persist request records; empty = no persistence
	RequestLogMaxMB       int      `yaml:"request_log_max_mb"`       // Rotation size per file (MB); 0 = default 50. Retention = this × backups must exceed settlement_interval × peak rate.
	RequestLogBackups     int      `yaml:"request_log_backups"`      // Numbered backups kept; 0 = default 10
	// Stream resume (B2): when a stream is interrupted SERVER-side (mining yield,
	// engine crash) and the client is still connected, transparently continue the
	// generation on another capable worker and splice the streams — instead of
	// surfacing an error event mid-conversation. Requires workers that advertise the
	// "continuation" feature (M1 ≥ this release); older workers are simply never
	// resumed onto. Off by default.
	StreamResume     bool `yaml:"stream_resume"`
	StreamMaxResumes int  `yaml:"stream_max_resumes"` // per-request continuation budget; 0 = default 2

	// Abuse controls (B5). All optional; 0 disables that dimension.
	RatePerSecPerKey    float64 `yaml:"rate_per_sec_per_key"`   // Sustained requests/sec per API key; 0 = unlimited
	RateBurstPerKey     int     `yaml:"rate_burst_per_key"`     // Token-bucket burst size; 0 = ceil(rate) or 1
	MaxConcurrentPerKey int     `yaml:"max_concurrent_per_key"` // Max in-flight requests per API key; 0 = unlimited
	MaxRequestBytes     int64   `yaml:"max_request_bytes"`      // Max request body size in bytes; 0 = default 10 MiB
}

// APIKey represents a client API key with metadata for billing (M3 prep).
type APIKey struct {
	Key    string `yaml:"key"`
	Name   string `yaml:"name"`             // Human-readable label
	Wallet string `yaml:"wallet,omitempty"` // Filecoin wallet address (M3)
}

type WorkerConfig struct {
	PollIntervalSec      int `yaml:"poll_interval_sec"`
	OfflineTimeoutSec    int `yaml:"offline_timeout_sec"`
	OfflineFailThreshold int `yaml:"offline_fail_threshold"` // Consecutive poll failures before offline
	// PollTimeoutSec bounds each /health and /ready poll request. 0 = built-in 5s
	// (fine on a LAN). Over the public internet (higher RTT/jitter) raise it (e.g. 10)
	// and raise offline_fail_threshold (e.g. 5) so a transient blip doesn't flap a
	// healthy worker offline. Keep it < poll_interval_sec so polls don't overlap.
	PollTimeoutSec int `yaml:"poll_timeout_sec"`
}

type AdminConfig struct {
	Port  int    `yaml:"port"`
	Token string `yaml:"token"` // Bearer token for /api/v1/* endpoints; empty = auth disabled
}

// PublicQueryConfig controls the separate, read-only, NO-AUTH public query port that
// exposes SP per-request earnings (transparency). It never exposes the admin token's
// powers (register/settle/pause) and never carries client identity. Disabled by
// default — opt in. Requires settlement.enabled (the earnings data comes from the
// settlement engine's ledger + request log).
type PublicQueryConfig struct {
	Enabled    bool    `yaml:"enabled"`      // default false
	Port       int     `yaml:"port"`         // default 9092
	RatePerSec float64 `yaml:"rate_per_sec"` // global request rate; 0 = default 20
	RateBurst  int     `yaml:"rate_burst"`   // token-bucket burst; 0 = 2×rate
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
	cfg.Settlement.ApplyDefaults()

	if err := validate(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.Settlement.Validate(); err != nil {
		return nil, fmt.Errorf("settlement config: %w", err)
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
	// Fail-closed in production: refuse to start with authentication disabled,
	// rather than silently exposing the admin and gateway APIs (G1).
	if cfg.Mode == "prod" {
		if cfg.Admin.Token == "" {
			return fmt.Errorf("admin.token is required in prod mode (refusing to start with admin auth disabled)")
		}
		if cfg.Gateway.ClientToken == "" && len(cfg.Gateway.APIKeys) == 0 {
			return fmt.Errorf("gateway.client_token or gateway.api_keys is required in prod mode (refusing to start with client auth disabled)")
		}
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
	if cfg.PublicQuery.Port == 0 {
		cfg.PublicQuery.Port = 9092
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
}
