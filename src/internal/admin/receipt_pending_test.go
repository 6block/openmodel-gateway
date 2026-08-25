package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
)

// The receipt-proof endpoint used to collapse "not settled yet" and "no such
// request" into one 404 error string — and the moment a user is MOST likely to
// open their receipt link is right after the reply, i.e. always before the
// batch. These tests pin the three distinct answers.
func setupReceiptAPI(t *testing.T, billedLine string) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scfg := &settlement.Config{
		IntervalMinutes: 20,
		ModelPricesUSD:  map[string]string{"default": "1000000"},
		FILPriceUSD:     "2.0", FILPriceSource: "manual",
	}
	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(nil, nil, pricer, 30, logger)
	dataDir := t.TempDir()
	reqLog := filepath.Join(dataDir, "requests.jsonl")
	if billedLine != "" {
		if err := os.WriteFile(reqLog, []byte(billedLine+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	settler := settlement.NewSettler(scfg, nil, pricer, bc, nil, reqLog, dataDir, logger)
	sa := NewSettlementAPI(nil, bc, pricer, settler, nil, nil, map[string]string{}, logger)
	mux := http.NewServeMux()
	sa.RegisterPublicRoutes(mux)
	return httptest.NewServer(mux)
}

func TestReceiptProof_PendingIsNotAnError(t *testing.T) {
	line := `{"request_id":"req-abc123","timestamp":"2026-07-30T10:00:00Z","model":"m","total_tokens":42,` +
		`"receipt":{"v":1,"request_id":"req-abc123","sig":"aa","pubkey":"bb","verified":true}}`
	srv := setupReceiptAPI(t, line)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/receipt-proof/req-abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("billed-but-unsettled must be 202, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("202 must carry Retry-After so clients know when to come back")
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "pending_settlement" || body["settled"] != false {
		t.Fatalf("pending body must be self-describing, got %v", body)
	}
	if body["worker_receipt"] == nil {
		t.Fatal("the immediately-verifiable worker receipt must be included")
	}
	if _, ok := body["error"]; ok {
		t.Fatal("a pending settlement is a state, not an error")
	}
}

func TestReceiptProof_UnknownIsStill404(t *testing.T) {
	srv := setupReceiptAPI(t, "")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/receipt-proof/req-nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an id with no billing record must stay 404, got %d", resp.StatusCode)
	}
}

// The receipt viewer fetches the public-query origin from the web UI's origin —
// cross-origin in every real deployment (same host, different port). Without
// CORS the browser silently blocks it and the viewer reads "could not reach",
// which is exactly how the regression shipped: the old <a> link was a
// navigation (CORS-exempt), the new in-page fetch is not.
func TestReceiptProof_CORSForBrowserFetch(t *testing.T) {
	srv := setupReceiptAPI(t, "")
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/v1/receipt-proof/req-x", nil)
	req.Header.Set("Origin", "https://openmodel.filfox.info")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("preflight must allow the web UI origin")
	}

	get, err := http.Get(srv.URL + "/api/v1/receipt-proof/req-x")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("actual GET responses must carry CORS headers too")
	}
	if get.Header.Get("Access-Control-Expose-Headers") != "Retry-After" {
		t.Fatal("Retry-After must be exposed so scripted clients can read the ETA")
	}
}
