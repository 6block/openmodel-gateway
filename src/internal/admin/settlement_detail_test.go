package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"openmodel/sp-state-agent/internal/settlement"
)

// mockReader implements admin.ContractReader with canned on-chain data.
type mockReader struct {
	nonce     uint64
	rec       settlement.OnChainSettlement
	processed bool
}

func (m *mockReader) GetUserBalance(ctx context.Context, user, token common.Address) (*big.Int, error) {
	return big.NewInt(0), nil
}
func (m *mockReader) GetSPEarnings(ctx context.Context, sp, token common.Address) (*big.Int, error) {
	return big.NewInt(0), nil
}
func (m *mockReader) SettlementNonce(ctx context.Context) (uint64, error) { return m.nonce, nil }
func (m *mockReader) GetSettlement(ctx context.Context, id uint64) (settlement.OnChainSettlement, error) {
	return m.rec, nil
}
func (m *mockReader) IsProcessedBatch(ctx context.Context, h [32]byte) (bool, error) {
	return m.processed, nil
}
func (m *mockReader) OperatorBalance(ctx context.Context) (*big.Int, error) { return big.NewInt(0), nil }
func (m *mockReader) OperatorAddress() common.Address                       { return common.Address{} }
func (m *mockReader) PlatformFeeBps(ctx context.Context) (int64, error)     { return 300, nil }

func setupDetailAPI(t *testing.T, mock *mockReader, withLocalAudit bool) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scfg := &settlement.Config{
		ModelPricesUSD:    map[string]string{"default": "1000000"},
		FILPriceUSD:       "2.0",
		FILPriceSource:    "manual",
		SupportedTokens:   []settlement.TokenConfig{{Symbol: "USDC", Address: "0x0000000000000000000000000000000000000001", Decimals: 6}},
		DeductionPriority: []string{"USDC"},
	}
	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(nil, scfg.SupportedTokens, pricer, 30, logger)
	dataDir := t.TempDir()
	reqLog := filepath.Join(dataDir, "requests.jsonl")
	settler := settlement.NewSettler(scfg, nil, pricer, bc, nil, reqLog, dataDir, logger)

	if withLocalAudit {
		hashNo0x := hex.EncodeToString(mock.rec.DetailsHash[:])
		line := `{"tx_hash":"0xTX","block_number":8,"gas_used":123,"item_count":3,"details_hash":"` +
			hashNo0x + `","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
		if err := os.WriteFile(filepath.Join(dataDir, "settlements.jsonl"), []byte(line), 0644); err != nil {
			t.Fatal(err)
		}
	}

	sa := NewSettlementAPI(mock, bc, pricer, settler, nil, scfg.SupportedTokens, map[string]string{}, logger)
	mux := http.NewServeMux()
	sa.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func getJSON(t *testing.T, url string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestHandleSettlementByID(t *testing.T) {
	var dh [32]byte
	for i := range dh {
		dh[i] = 0xAB
	}
	mock := &mockReader{
		nonce: 5,
		rec: settlement.OnChainSettlement{
			BatchId:      big.NewInt(3),
			Timestamp:    big.NewInt(1234),
			TotalAmount:  big.NewInt(5000),
			SettledCount: big.NewInt(3),
			FailedCount:  big.NewInt(0),
			DetailsHash:  dh,
		},
		processed: true,
	}
	srv := setupDetailAPI(t, mock, true)
	defer srv.Close()

	st, body := getJSON(t, srv.URL+"/api/v1/settlements/3")
	if st != 200 {
		t.Fatalf("expected 200, got %d (%v)", st, body)
	}
	if body["batch_id"].(float64) != 3 {
		t.Errorf("batch_id = %v, want 3", body["batch_id"])
	}
	onChain := body["on_chain"].(map[string]interface{})
	if onChain["processed"] != true {
		t.Errorf("processed = %v, want true", onChain["processed"])
	}
	if onChain["settled_count"].(float64) != 3 {
		t.Errorf("settled_count = %v, want 3", onChain["settled_count"])
	}
	wantHash := "0x" + hex.EncodeToString(dh[:])
	if onChain["details_hash"] != wantHash {
		t.Errorf("details_hash = %v, want %s", onChain["details_hash"], wantHash)
	}
	if onChain["total_amount"] != "5000" {
		t.Errorf("total_amount = %v, want \"5000\"", onChain["total_amount"])
	}
	local := body["local_audit"].(map[string]interface{})
	if local["found"] != true || local["tx_hash"] != "0xTX" || local["block_number"].(float64) != 8 {
		t.Errorf("local_audit mismatch: %v", local)
	}
}

func TestHandleSettlementByID_NotFoundAndBadID(t *testing.T) {
	mock := &mockReader{nonce: 5}
	srv := setupDetailAPI(t, mock, false)
	defer srv.Close()

	// id beyond the latest nonce → 404
	if st, _ := getJSON(t, srv.URL+"/api/v1/settlements/99"); st != 404 {
		t.Errorf("expected 404 for id>nonce, got %d", st)
	}
	// non-numeric id → 400
	if st, _ := getJSON(t, srv.URL+"/api/v1/settlements/abc"); st != 400 {
		t.Errorf("expected 400 for non-numeric id, got %d", st)
	}
	// zero id → 400
	if st, _ := getJSON(t, srv.URL+"/api/v1/settlements/0"); st != 400 {
		t.Errorf("expected 400 for id 0, got %d", st)
	}
}

// TestHandleSettlementByID_NoLocalRecord: on-chain processed but the local audit
// log has no matching entry → local_audit.found=false (the audit-gap signal).
func TestHandleSettlementByID_NoLocalRecord(t *testing.T) {
	var dh [32]byte
	for i := range dh {
		dh[i] = 0xCD
	}
	mock := &mockReader{
		nonce:     2,
		rec:       settlement.OnChainSettlement{BatchId: big.NewInt(1), Timestamp: big.NewInt(1), TotalAmount: big.NewInt(10), SettledCount: big.NewInt(1), FailedCount: big.NewInt(0), DetailsHash: dh},
		processed: true,
	}
	srv := setupDetailAPI(t, mock, false) // no local audit file
	defer srv.Close()

	st, body := getJSON(t, srv.URL+"/api/v1/settlements/1")
	if st != 200 {
		t.Fatalf("expected 200, got %d", st)
	}
	local := body["local_audit"].(map[string]interface{})
	if local["found"] != false {
		t.Errorf("expected local_audit.found=false, got %v", local)
	}
}
