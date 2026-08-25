package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

const (
	billWallet = "0x00000000000000000000000000000000000000B1"
	billUSDC   = "0x0000000000000000000000000000000000000001"
	billFIL    = "0x0000000000000000000000000000000000000000"
)

func usdcWei(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)) }

// fakeBalanceContract implements settlement's balanceContract interface, returning
// a canned USDC balance so BalanceCache.ForceRefresh seeds chainBalances without a
// real chain.
type fakeBalanceContract struct{ usdc *big.Int }

func (f *fakeBalanceContract) GetUserBalance(ctx context.Context, user, token common.Address) (*big.Int, error) {
	if token == common.HexToAddress(billUSDC) {
		return new(big.Int).Set(f.usdc), nil
	}
	return big.NewInt(0), nil
}

// newBillingGateway wires a Gateway with settlement billing enabled: a registry with
// one idle worker per endpoint, a BalanceCache seeded with `balanceUSDC`, $1/token
// pricing, and a single API key bound to billWallet.
func newBillingGateway(t *testing.T, balanceUSDC *big.Int, endpoints ...string) (*httptest.Server, *settlement.BalanceCache, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := worker.NewRegistry(logger, "")
	for i, ep := range endpoints {
		id := fmt.Sprintf("w%d", i)
		registry.Register(worker.WorkerRegistration{ID: id, Endpoint: ep, SchedulerURL: ep, GPUCount: 1})
		registry.UpdateState(id, "GPU_STATE_AVAILABLE", "running", 0, "test-model", 1)
	}

	scfg := &settlement.Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1 per token
		FILPriceUSD:    "2.0",
		FILPriceSource: "manual",
		SupportedTokens: []settlement.TokenConfig{
			{Symbol: "USDC", Address: billUSDC, Decimals: 6},
			{Symbol: "FIL", Address: billFIL, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		DefaultMaxTokens:  100,
	}
	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: balanceUSDC}, scfg.SupportedTokens, pricer, 30, logger)
	bc.SetWallets([]string{billWallet})
	bc.ForceRefresh(context.Background()) // seed chainBalances from the fake

	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1", Wallet: billWallet}},
	}, logger)
	gw.SetBalanceChecker(bc, scfg)
	srv := httptest.NewServer(gw.Handler())
	return srv, bc, func() { srv.Close(); _ = gw.Close() }
}

func doChat(t *testing.T, gwURL, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", gwURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Errorf("new request: %v", err)
		return 0
	}
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("request: %v", err)
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func sseServer(chunks ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprint(w, c)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
}

const chatBody = `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":10}`

// TestBilling_NonStreamingSuccessBillsActual: reserve by max_tokens, then settle by
// the actual usage returned (estimated $10 → actual $8).
func TestBilling_NonStreamingSuccessBillsActual(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":2,"completion_tokens":6,"total_tokens":8}}`)
	}))
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	if st := doChat(t, gw.URL, chatBody); st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(big.NewFloat(8)) != 0 {
		t.Errorf("expected pendingSpend $8 (actual tokens billed), got %s", ps.Text('f', 4))
	}
}

// TestBilling_NonStreamingRetryThenSuccess: first worker 503, retry on the second
// succeeds; the client sees 200 and is billed exactly once for the actual usage.
func TestBilling_NonStreamingRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"mining"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":2,"completion_tokens":6,"total_tokens":8}}`)
	}))
	defer up.Close()
	// two workers share the stateful upstream so the retry has a target to land on
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL, up.URL)
	defer cleanup()

	if st := doChat(t, gw.URL, chatBody); st != 200 {
		t.Fatalf("expected 200 after retry, got %d", st)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 upstream calls (503 then 200), got %d", got)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(big.NewFloat(8)) != 0 {
		t.Errorf("expected pendingSpend $8 billed once after retry, got %s", ps.Text('f', 4))
	}
}

// TestBilling_AllRetriesFailReversesReservation: every attempt 503 → client gets 503
// and the reservation is fully reversed (no phantom charge).
func TestBilling_AllRetriesFailReversesReservation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"mining"}`)
	}))
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	if st := doChat(t, gw.URL, chatBody); st != 503 {
		t.Fatalf("expected 503, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("expected pendingSpend reversed to 0 on total failure, got %s", ps.Text('f', 4))
	}
}

// TestBilling_StreamingBilledByUsageChunk: streaming usage comes from a usage SSE
// chunk scanned out of the byte stream.
func TestBilling_StreamingBilledByUsageChunk(t *testing.T) {
	up := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":6,\"total_tokens\":8}}\n\n",
		"data: [DONE]\n\n",
	)
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":10,"stream":true}`
	if st := doChat(t, gw.URL, body); st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(big.NewFloat(8)) != 0 {
		t.Errorf("expected streaming billed $8 from usage chunk, got %s", ps.Text('f', 4))
	}
}

// TestBilling_StreamInterruptedNotBilled: an error event mid-stream (mining) → the
// request is NOT billed (reservation fully reversed), prioritizing user experience.
func TestBilling_StreamInterruptedNotBilled(t *testing.T) {
	up := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"error\":{\"message\":\"Engine paused during generation\"}}\n\n",
	)
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":10,"stream":true}`
	doChat(t, gw.URL, body)
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("interrupted stream must NOT be billed, got pendingSpend %s", ps.Text('f', 4))
	}
}

// TestBilling_StreamNoUsageNotBilled: a clean stream with no usage chunk bills 0.
// A stream that DELIVERS content but never sends a final usage chunk (client
// disconnected before it, or usage wasn't requested) is now billed by GATEWAY-METERED
// delivered tokens — closing the "abort right before the usage chunk to get free tokens"
// hole. (Server-side interruptions that emit an error event are still not billed.)
func TestBilling_StreamNoUsageBilledByMeteredDelivery(t *testing.T) {
	up := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":10,"stream":true}`
	if st := doChat(t, gw.URL, body); st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() <= 0 {
		t.Errorf("delivered content without a usage chunk must be billed by metered delivery, got %s", ps.Text('f', 4))
	}
}

// A stream that delivers NO content and no usage bills nothing — there is nothing to charge.
func TestBilling_StreamNoContentNoUsageNotBilled(t *testing.T) {
	up := sseServer("data: [DONE]\n\n")
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(1000), up.URL)
	defer cleanup()

	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":10,"stream":true}`
	if st := doChat(t, gw.URL, body); st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("no content delivered → bill 0, got %s", ps.Text('f', 4))
	}
}

// TestBilling_402InsufficientBalance: zero balance → 402, and no pendingSpend left.
func TestBilling_402InsufficientBalance(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, big.NewInt(0), up.URL)
	defer cleanup()

	if st := doChat(t, gw.URL, chatBody); st != 402 {
		t.Fatalf("expected 402, got %d", st)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("402 must not leave pendingSpend, got %s", ps.Text('f', 4))
	}

	// 401: missing Authorization header.
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

// TestBilling_ConcurrentOverspendEndToEnd: a $5 balance with 25 concurrent $1
// requests — Reserve must be atomic end-to-end so no more than 5 succeed and
// pendingSpend never exceeds the balance. Run under -race.
func TestBilling_ConcurrentOverspendEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":0,"completion_tokens":1,"total_tokens":1}}`)
	}))
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(5), up.URL)
	defer cleanup()

	const n = 25
	var ok200, got402 atomic.Int32
	var wg sync.WaitGroup
	wg.Add(n)
	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":1}`
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			switch doChat(t, gw.URL, body) {
			case 200:
				ok200.Add(1)
			case 402:
				got402.Add(1)
			}
		}()
	}
	wg.Wait()

	if ok200.Load() > 5 {
		t.Errorf("OVERSPEND: %d requests succeeded on a $5 balance (max 5)", ok200.Load())
	}
	if ok200.Load()+got402.Load() != n {
		t.Errorf("expected all %d requests to be 200 or 402, got 200=%d 402=%d", n, ok200.Load(), got402.Load())
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Cmp(big.NewFloat(5)) > 0 {
		t.Errorf("pendingSpend %s exceeds the $5 balance (overspend)", ps.Text('f', 4))
	}
}
