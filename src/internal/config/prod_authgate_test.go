package config

import (
	"strings"
	"testing"
)

// A prod config whose only api_keys entry references an unset ${ENV} var used to
// pass validation: the check counted list entries, not keys with a value. The
// gateway then built an empty key table, switched auth off, and served a billing
// endpoint with no credentials required. Prod must refuse to start instead.
func TestLoad_ProdRejectsAPIKeysWithNoValue(t *testing.T) {
	t.Setenv("OM_TEST_UNSET_KEY", "") // guaranteed empty after interpolation
	content := "mode: prod\nadmin:\n  token: at\ngateway:\n  api_keys:\n" +
		"    - key: ${OM_TEST_UNSET_KEY}\n      name: client1\n"
	_, err := Load(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("prod accepted api_keys whose only entry has an empty key — client auth would be OFF")
	}
	if !strings.Contains(err.Error(), "none carry a value") {
		t.Fatalf("expected the empty-key explanation, got: %v", err)
	}
}

func TestLoad_ProdAcceptsAPIKeyWithValue(t *testing.T) {
	content := "mode: prod\nadmin:\n  token: at\ngateway:\n  api_keys:\n" +
		"    - key: sk-om-real\n      name: client1\n"
	if _, err := Load(writeTempConfig(t, content)); err != nil {
		t.Fatalf("prod rejected a valid key: %v", err)
	}
}

// One empty entry alongside a real one is fine — the real one carries auth.
func TestLoad_ProdAcceptsMixedKeys(t *testing.T) {
	t.Setenv("OM_TEST_UNSET_KEY", "")
	content := "mode: prod\nadmin:\n  token: at\ngateway:\n  api_keys:\n" +
		"    - key: ${OM_TEST_UNSET_KEY}\n      name: unset\n" +
		"    - key: sk-om-real\n      name: client2\n"
	if _, err := Load(writeTempConfig(t, content)); err != nil {
		t.Fatalf("prod rejected a config that has one usable key: %v", err)
	}
}
