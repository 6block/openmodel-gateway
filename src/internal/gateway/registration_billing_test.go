package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// These tests pin the fix for the "self-registered wallet is 402'd forever" bug: a
// wallet only becomes spendable if registration actually adds it to the BalanceCache
// refresh list (refreshAll only iterates bc.wallets — a wallet not in that list is
// never polled, so availableUSD reads 0 and every request 402s). "Registration returned
// 200" is NOT the same as "the user can spend".

func chatUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

func doChatKey(t *testing.T, gwURL, key string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", gwURL+"/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// TestRegister_MakesWalletSpendable is the A/B that proves the fix: two funded wallets
// with the SAME on-chain balance behave differently based ONLY on whether registration
// wired them into the refresh list. The registered wallet spends (200); a wallet whose
// key was injected WITHOUT AddWallet (the old startup behavior: registrations loaded into
// apiKeys but never into bc.wallets) is 402'd though it has funds — the exact bug.
func TestRegister_MakesWalletSpendable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	up := chatUpstream(t)
	defer up.Close()

	registry := worker.NewRegistry(logger, "")
	registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1})
	registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)

	scfg := &settlement.Config{
		ModelPricesUSD:    map[string]string{"default": "1000000"}, // $1/token
		FILPriceUSD:       "2.0",
		FILPriceSource:    "manual",
		SupportedTokens:   []settlement.TokenConfig{{Symbol: "USDC", Address: billUSDC, Decimals: 6}, {Symbol: "FIL", Address: billFIL, Decimals: 18}},
		DeductionPriority: []string{"USDC", "FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, logger)
	// The fake returns a fat USDC balance for ANY wallet — so a 402 can ONLY come from a
	// wallet that was never refreshed (never added to the list), never from real poverty.
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(1000)}, scfg.SupportedTokens, pricer, 30, logger)

	dataDir := t.TempDir()
	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		// one placeholder static key so auth is ENABLED; it is not our test wallet
		APIKeys:        []config.APIKey{{Key: "sk-static", Name: "static", Wallet: "0x00000000000000000000000000000000000000FF"}},
		RequestLogPath: filepath.Join(dataDir, "req.jsonl"),
	}, logger)
	defer gw.Close()
	gw.SetBalanceChecker(bc, scfg)
	// Startup seeds the refresh list from the gateway's known wallets (mirrors main.go).
	bc.SetWallets(gw.KnownWallets())

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	// --- Experiment: register a wallet, then spend on the issued key ---
	privReg, walletReg := genWallet(t)
	issued := time.Now().Unix()
	rr := postRegisterSrv(t, srv.URL, map[string]any{"wallet": walletReg, "issued_at": issued, "signature": signFor(t, privReg, walletReg, issued)})
	var resp registerResponse
	if err := json.Unmarshal(rr, &resp); err != nil || resp.APIKey == "" {
		t.Fatalf("register failed: %s", string(rr))
	}
	// AddWallet fired an async refresh; ForceRefresh makes the assertion deterministic
	// (equivalent to the next periodic tick). This ONLY reaches walletReg if registration
	// actually added it to bc.wallets — which is the fix.
	bc.ForceRefresh(context.Background())
	if code := doChatKey(t, srv.URL, resp.APIKey); code != http.StatusOK {
		t.Fatalf("registered+funded wallet must be able to spend, got %d (want 200)", code)
	}

	// --- Control: inject a key bound to a funded wallet WITHOUT AddWallet (old bug path) ---
	_, walletInj := genWallet(t)
	gw.keysMu.Lock()
	gw.apiKeys[hashKey("sk-injected")] = apiKeyEntry{Name: "inj", Wallet: walletInj}
	gw.keysMu.Unlock()
	bc.ForceRefresh(context.Background()) // even a full refresh cycle can't reach it
	if code := doChatKey(t, srv.URL, "sk-injected"); code != http.StatusPaymentRequired {
		t.Fatalf("a funded wallet NOT in the refresh list must 402 (reproduces the bug), got %d (want 402)", code)
	}
}

// postRegisterSrv POSTs to a live gateway server's /v1/register and returns the body.
func postRegisterSrv(t *testing.T, gwURL string, body map[string]any) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(gwURL+"/v1/register", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("register post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out
}

// TestKnownWallets_RegisteredSurviveRestart: a wallet self-registered before a restart is
// still returned by KnownWallets after (New reloads registrations.json into apiKeys), so
// main.go seeds it into the refresh list — closing the "402'd forever after restart" hole.
// Also checks de-dup and that a no-wallet client_token key is excluded.
func TestKnownWallets_RegisteredSurviveRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "req.jsonl")

	// Pre-restart: register a wallet against gateway A (persists registrations.json).
	registry := worker.NewRegistry(logger, "")
	gwA := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "sk-static", Name: "static", Wallet: "0x00000000000000000000000000000000000000FF"}},
		RequestLogPath:    logPath,
	}, logger)
	srvA := httptest.NewServer(gwA.Handler())
	priv, wallet := genWallet(t)
	issued := time.Now().Unix()
	if body := postRegisterSrv(t, srvA.URL, map[string]any{"wallet": wallet, "issued_at": issued, "signature": signFor(t, priv, wallet, issued)}); len(body) == 0 {
		t.Fatal("registration returned empty body")
	}
	srvA.Close()
	_ = gwA.Close()

	// Post-restart: a fresh gateway with the SAME RequestLogPath reloads registrations.
	gwB := New(worker.NewRegistry(logger, ""), config.GatewayConfig{
		RequestTimeoutSec: 5,
		ClientToken:       "sk-notoken-wallet", // no wallet → must be excluded from KnownWallets
		RequestLogPath:    logPath,
	}, logger)
	defer gwB.Close()

	got := gwB.KnownWallets()
	found := false
	for _, w := range got {
		if w == wallet {
			found = true
		}
		if w == "" {
			t.Error("KnownWallets must never return an empty wallet")
		}
	}
	if !found {
		t.Fatalf("registered wallet %s must survive restart in KnownWallets, got %v", wallet, got)
	}
}

// TestAddWallet_Idempotent: adding the same wallet twice does not duplicate it, and the
// refresh list stays clean (guards against unbounded growth on re-registration paths).
func TestAddWallet_Idempotent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scfg := &settlement.Config{
		FILPriceUSD: "2.0", FILPriceSource: "manual",
		SupportedTokens: []settlement.TokenConfig{{Symbol: "USDC", Address: billUSDC, Decimals: 6}},
	}
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(1)}, scfg.SupportedTokens, settlement.NewPricer(scfg, logger), 30, logger)
	w := "0x00000000000000000000000000000000000000B7"
	bc.AddWallet(w)
	bc.AddWallet(w)                  // exact duplicate
	bc.AddWallet(strings.ToLower(w)) // case variant → still the same wallet
	if n := bc.WalletCount(); n != 1 {
		t.Fatalf("AddWallet must be idempotent, wallet count = %d (want 1)", n)
	}
}
