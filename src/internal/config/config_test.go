package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	content := `mode: dev`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Mode != "dev" {
		t.Errorf("mode: want dev, got %s", cfg.Mode)
	}
	if cfg.Gateway.Port != 3000 {
		t.Errorf("gateway.port default: want 3000, got %d", cfg.Gateway.Port)
	}
	if cfg.Gateway.RequestTimeoutSec != 120 {
		t.Errorf("gateway.request_timeout_sec default: want 120, got %d", cfg.Gateway.RequestTimeoutSec)
	}
	if cfg.Workers.PollIntervalSec != 5 {
		t.Errorf("poll_interval_sec default: want 5, got %d", cfg.Workers.PollIntervalSec)
	}
	if cfg.Workers.OfflineFailThreshold != 3 {
		t.Errorf("offline_fail_threshold default: want 3, got %d", cfg.Workers.OfflineFailThreshold)
	}
	if cfg.Admin.Port != 9091 {
		t.Errorf("admin.port default: want 9091, got %d", cfg.Admin.Port)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level default: want info, got %s", cfg.Logging.Level)
	}
}

func TestLoad_EnvInterpolation(t *testing.T) {
	t.Setenv("TEST_TOKEN_XYZ", "secret-from-env")

	content := `
mode: dev
gateway:
  client_token: ${TEST_TOKEN_XYZ}
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Gateway.ClientToken != "secret-from-env" {
		t.Errorf("env interpolation: want secret-from-env, got %s", cfg.Gateway.ClientToken)
	}
}

func TestLoad_EnvInterpolation_Default(t *testing.T) {
	os.Unsetenv("UNSET_VAR_12345")

	content := `
mode: dev
gateway:
  port: 3000
  client_token: ${UNSET_VAR_12345:-fallback-token}
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Gateway.ClientToken != "fallback-token" {
		t.Errorf("default value: want fallback-token, got %s", cfg.Gateway.ClientToken)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	content := `mode: [invalid yaml`
	path := writeTempConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error for invalid YAML")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// G1: prod mode must fail-closed when authentication is not configured.
func TestLoad_ProdRequiresAdminToken(t *testing.T) {
	content := "mode: prod\ngateway:\n  client_token: ct\n"
	_, err := Load(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("G1 regression: prod mode loaded with empty admin.token")
	}
}

func TestLoad_ProdRequiresClientAuth(t *testing.T) {
	content := "mode: prod\nadmin:\n  token: at\n"
	_, err := Load(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("G1 regression: prod mode loaded with no gateway client auth")
	}
}

func TestLoad_ProdWithAuthSucceeds(t *testing.T) {
	content := "mode: prod\nadmin:\n  token: at\ngateway:\n  client_token: ct\n"
	if _, err := Load(writeTempConfig(t, content)); err != nil {
		t.Fatalf("prod mode with full auth should load, got: %v", err)
	}
}

func TestLoad_DevAllowsNoAuth(t *testing.T) {
	// Dev mode keeps the convenience of running without tokens.
	if _, err := Load(writeTempConfig(t, "mode: dev")); err != nil {
		t.Fatalf("dev mode without auth should load, got: %v", err)
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
