package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"openmodel/sp-state-agent/internal/settlement"
)

type Config struct {
	Mode           string               `yaml:"mode"` // "dev" or "prod"
	Gateway        GatewayConfig        `yaml:"gateway"`
	Workers        WorkerConfig         `yaml:"workers"`
	Admin          AdminConfig          `yaml:"admin"`
	PublicQuery    PublicQueryConfig    `yaml:"public_query"`
	Verification   VerificationConfig   `yaml:"verification"`
	SPRegistration SPRegistrationConfig `yaml:"sp_registration"`
	Ban            BanConfig            `yaml:"ban"`
	Probe          ProbeConfig          `yaml:"probe"`
	WorkerMTLS     WorkerMTLSConfig     `yaml:"worker_mtls"`
	Logging        LoggingConfig        `yaml:"logging"`
	Metrics        MetricsConfig        `yaml:"metrics"`
	Settlement     settlement.Config    `yaml:"settlement"`
}

// SPRegistrationConfig controls SP (worker) self-registration on the public port:
// a miner proves control of its owner/worker key over a server challenge, passes
// the admission thresholds, and receives a per-worker auth token — no operator
// action involved.
type SPRegistrationConfig struct {
	Enabled bool `yaml:"enabled"` // default false (admin-API registration only)
	// GatewayID is the identity string SPs bind their signature to (goes verbatim
	// into the signed message), so a signature for this gateway cannot be replayed
	// against another. Required when enabled. Use a stable public name, e.g.
	// "openmodel-gateway-test".
	GatewayID string `yaml:"gateway_id"`
	// FilecoinRPCURLs are native Lotus JSON-RPC endpoints (NOT the FEVM eth
	// endpoint) used to read miner state. Multiple endpoints are cross-checked
	// against fake-empty responses; list at least two independent providers.
	FilecoinRPCURLs []string `yaml:"filecoin_rpc_urls"`
	// MinRawPowerBytes is the admission floor on the miner's raw byte power
	// (storage scale). "0"/empty disables the check. Decimal string, e.g.
	// "34359738368" = 32 GiB.
	MinRawPowerBytes string `yaml:"min_raw_power_bytes"`
	// MinMinerBalanceFIL is the admission floor on the miner actor's balance
	// (pledge + vesting rewards + available — evidence of real mining stake), in
	// FIL. "0"/empty disables the check. Decimal string, e.g. "10.5".
	MinMinerBalanceFIL string `yaml:"min_miner_balance_fil"`
	ChallengeTTLSec    int    `yaml:"challenge_ttl_sec"`     // default 600
	RegisterRatePerMin int    `yaml:"register_rate_per_min"` // per-IP limit on challenge+register; default 6
	MaxRegisteredSPs   int    `yaml:"max_registered_sps"`    // total worker cap; default 1000
	// Certificate-at-registration: when both paths are set, a CSR submitted with
	// a successful registration is signed on the spot and renewals are served on
	// /v1/sp/renew-cert (refused for banned workers — short-lived certs make
	// renewal the revocation mechanism). Empty = no issuing; manual/plaintext
	// workers unaffected. The CA key gets the same hot-key treatment as the
	// settlement operator key; production shape is a cold root + short-lived
	// intermediate here.
	IssuerCACert string `yaml:"issuer_ca_cert"`
	// HTTPSPort, when >0 and the issuer CA is configured, serves the SAME routes
	// over TLS with a gateway server certificate signed by that CA. This is the
	// worker→gateway direction's encryption (registration, token issuance, the
	// admission self-view): workers verify ServerName == gateway_id against the
	// CA they already trust, so no public PKI or domain is needed.
	HTTPSPort       int    `yaml:"https_port"`
	IssuerCAKey     string `yaml:"issuer_ca_key"`
	CertValiditySec int    `yaml:"cert_validity_sec"` // default 604800 (7 days)
}

// BanConfig controls the routing-ban punishment lever.
type BanConfig struct {
	DefaultDurationSec int `yaml:"default_duration_sec"` // used when an admin ban request omits duration; default 604800 (7 days)
}

// ProbeConfig controls the automated capability spot-check auditor. It periodically
// probes a servable self-registered worker with fresh deterministically-scored
// questions and auto-bans one scoring far below its claimed model's capability (the
// routing-ban lever; on-chain confiscation stays a manual arbiter decision).
type ProbeConfig struct {
	Enabled      bool    `yaml:"enabled"`
	IntervalSec  int     `yaml:"interval_sec"`  // seconds between probing one worker; 0 disables
	NumQuestions int     `yaml:"num_questions"` // questions per probe; 0 = default 16
	MinScore     float64 `yaml:"min_score"`     // suspect if score below this floor; 0 = default 0.5
	BanSeconds   int     `yaml:"ban_seconds"`   // ban duration on a suspect verdict; 0 = default 3600
	// AdmissionGate makes every claimed model prove itself before routing trusts
	// it: a freshly self-registered worker gets no traffic until its first
	// admission probe passes. Requires Enabled (a gate with no prober would
	// starve every worker forever — startup rejects that combination).
	AdmissionGate bool               `yaml:"admission_gate"`
	VerifyTTLSec  int                `yaml:"verify_ttl_sec"` // re-verify interval; 0 = default 604800 (7 days)
	ModelFloors   map[string]float64 `yaml:"model_floors"`   // claimed model → score floor; unlisted → min_score
	// SpotMinIntervalSec is the per-worker cooldown between routine spot checks
	// (0 = default 259200, i.e. 3 days). It exists because the spot-check tick
	// picks from the SELF-REGISTERED pool only: with few external workers, a
	// bare interval_sec cadence concentrates every probe on the same machines.
	// Admission probes and TTL re-verification ignore this cooldown — a new
	// worker must still be verified immediately.
	SpotMinIntervalSec int `yaml:"spot_min_interval_sec"`
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

// WorkerMTLSConfig arms the gateway→worker direction (polling, forwarding,
// probing) with mutual TLS. All three paths set = enabled; all empty =
// plaintext (current behavior). The TLS material only engages for workers whose
// endpoints are https:// — plaintext workers keep working, so a fleet migrates
// worker by worker and rollback is flipping an endpoint back to http.
// Identity is pinned to the WORKER ID (certificate SAN), never to the address:
// tunnels and port maps stay transparent.
type WorkerMTLSConfig struct {
	CAFile   string `yaml:"ca_file"`   // private CA bundle that signed the worker certs
	CertFile string `yaml:"cert_file"` // this gateway's client certificate
	KeyFile  string `yaml:"key_file"`  // its private key
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

	// WebUI serves the embedded chat + wallet-registration single-page app on the
	// gateway port (same origin as the API, so no CORS surface). Off by default.
	WebUI WebUIConfig `yaml:"web_ui"`

	// Key-store v2 (self-service key management).
	MaxKeysPerWallet   int `yaml:"max_keys_per_wallet"`   // cap per wallet; 0 = default 10
	RegisterRatePerMin int `yaml:"register_rate_per_min"` // per-IP limit on /v1/register + /v1/keys; 0 = default 10
	// TrustedProxies lists reverse proxies (IPs or CIDRs) whose
	// X-Forwarded-For / X-Real-IP headers are believed for per-IP rate
	// limiting. Empty = headers ignored (direct peers only). Set this when the
	// gateway sits behind a domain front, or every user arriving through it
	// shares the proxy's single rate-limit bucket.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// WebUIConfig controls the embedded browser app (M4.1 user terminal).
type WebUIConfig struct {
	Enabled bool `yaml:"enabled"`
	// PublicQueryURL is the EXTERNAL base URL of the public read-only query port
	// (receipt-proof), as reachable by end users — e.g. "https://openmodel.filfox.info".
	// Display-only: the UI links receipts there. Empty hides verify links.
	PublicQueryURL string `yaml:"public_query_url"`
}

// APIKey represents a client API key with metadata for billing.
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
		// Count keys that actually carry a value: an api_keys entry whose `key`
		// is an unset ${ENV} placeholder expands to "" and is skipped when the
		// lookup table is built, which switches client auth OFF entirely. A
		// non-empty LIST is therefore not proof that auth is on.
		configured := 0
		for _, k := range cfg.Gateway.APIKeys {
			if k.Key != "" {
				configured++
			}
		}
		if cfg.Gateway.ClientToken == "" && configured == 0 {
			return fmt.Errorf("gateway.client_token or at least one api_keys entry with a non-empty key is required in prod mode (refusing to start with client auth disabled; %d api_keys entries present but none carry a value — check the ${ENV} vars they reference)", len(cfg.Gateway.APIKeys))
		}
	}
	if cfg.SPRegistration.Enabled {
		if cfg.SPRegistration.GatewayID == "" {
			return fmt.Errorf("sp_registration.gateway_id is required when sp_registration.enabled (signatures are domain-bound to it)")
		}
		if len(cfg.SPRegistration.FilecoinRPCURLs) == 0 {
			return fmt.Errorf("sp_registration.filecoin_rpc_urls is required when sp_registration.enabled (miner admission reads chain state)")
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
	if cfg.SPRegistration.ChallengeTTLSec == 0 {
		cfg.SPRegistration.ChallengeTTLSec = 600
	}
	if cfg.SPRegistration.RegisterRatePerMin == 0 {
		cfg.SPRegistration.RegisterRatePerMin = 6
	}
	if cfg.SPRegistration.MaxRegisteredSPs == 0 {
		cfg.SPRegistration.MaxRegisteredSPs = 1000
	}
	if cfg.Ban.DefaultDurationSec == 0 {
		cfg.Ban.DefaultDurationSec = 604800 // 7 days
	}
}
