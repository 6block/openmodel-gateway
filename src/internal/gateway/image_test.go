package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// An image-generation model billed per image. The unit here is the whole point: the
// request carries no tokens, so a token-shaped pipeline would price it at zero.
const imageModel = "flux-schnell"

func imageCfg() *settlement.Config {
	return &settlement.Config{
		ModelPricesUSD: map[string]string{"default": "1000000"}, // $1/token
		ModelCatalog: map[string]settlement.ModelInfo{
			imageModel: {
				UnitUSD: "2", Modality: settlement.ModalityImage,
				PriceUnit: settlement.PriceUnitImage, MaxImagesPerGeneration: 4,
			},
		},
		FILPriceUSD:    "2.0",
		FILPriceSource: "manual",
		SupportedTokens: []settlement.TokenConfig{
			{Symbol: "USDC", Address: billUSDC, Decimals: 6},
			{Symbol: "FIL", Address: billFIL, Decimals: 18},
		},
		DeductionPriority: []string{"USDC", "FIL"},
		DefaultMaxTokens:  100,
	}
}

// pngB64 is a real (tiny) PNG as base64, so the endpoint's decode-and-hash path is
// exercised rather than stubbed.
func pngB64(t *testing.T, seed int) string {
	t.Helper()
	raw := mockPNG(seed)
	return base64.StdEncoding.EncodeToString(raw)
}

// imageUpstream stands in for a worker's /v1/images/generations.
func imageUpstream(t *testing.T, images int, receiptSig string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			http.NotFound(w, r)
			return
		}
		var items []map[string]string
		for i := 0; i < images; i++ {
			items = append(items, map[string]string{"b64_json": pngB64(t, i)})
		}
		body, _ := json.Marshal(map[string]any{
			"created": 1, "data": items,
			"usage": map[string]int{"images": images},
		})
		w.Header().Set("Content-Type", "application/json")
		if receiptSig != "" {
			// Asset-shaped receipt: quantity + asset_hash, no token counts.
			h := hashImagesForTest(t, images)
			rcpt := map[string]any{
				"v": 1, "request_id": r.Header.Get("X-Request-ID"), "model": imageModel,
				"request_sha256": "aa", "response_sha256": h, "quantity": images,
				"asset_hash": h, "ts": 1, "pubkey": "pk", "sig": receiptSig,
			}
			b, _ := json.Marshal(rcpt)
			w.Header().Set("X-Om-Receipt", base64.StdEncoding.EncodeToString(b))
		}
		w.Write(body)
	}))
}

func newImageGateway(t *testing.T, up *httptest.Server, scfg *settlement.Config) (*Gateway, *settlement.BalanceCache) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := worker.NewRegistry(logger, "")
	registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1})
	registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, imageModel, 1)
	// An image-generation worker signs receipts like any other; the gateway only asks
	// when the worker advertises it.
	registry.SetFeatures("w0", []string{worker.FeatureReceipt, worker.FeatureImageGen}, "pk")

	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(1000)}, scfg.SupportedTokens, pricer, 30, logger)
	bc.SetWallets([]string{billWallet})
	bc.ForceRefresh(t.Context())

	gw := New(registry, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1", Wallet: billWallet}},
	}, logger)
	gw.SetBalanceChecker(bc, scfg)
	return gw, bc
}

func postImages(t *testing.T, gwURL, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", gwURL+"/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var parsed map[string]any
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

func imageBody(n int, model string) string {
	return fmt.Sprintf(`{"model":%q,"prompt":"a red cube","n":%d,"size":"512x512"}`, model, n)
}

func TestImageGeneration_BillsPerImage(t *testing.T) {
	up := imageUpstream(t, 2, "")
	defer up.Close()
	gw, bc := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, body := postImages(t, srv.URL, imageBody(2, imageModel))
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	// $2 per image × 2 = $4 pending, priced in the IMAGE unit.
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 2) != "4.00" {
		t.Errorf("pending must be $4.00 (2 images × $2), got %s", ps.Text('f', 6))
	}
}

func TestImageGeneration_RejectsTokenPricedModel(t *testing.T) {
	up := imageUpstream(t, 1, "")
	defer up.Close()
	cfg := imageCfg()
	cfg.ModelCatalog["some-text-model"] = settlement.ModelInfo{InputUSD: "0.20"}
	gw, _ := newImageGateway(t, up, cfg)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	// A token-priced model the catalog knows: the image endpoint must refuse it rather
	// than price images as tokens. (A model nothing knows is a 404 instead.)
	resp, body := postImages(t, srv.URL, imageBody(1, "some-text-model"))
	if resp.StatusCode != 400 {
		t.Fatalf("a token-priced model must be refused on the image endpoint, got %d", resp.StatusCode)
	}
	if e := fmt.Sprint(body["error"]); !strings.Contains(e, "is a text model") || !strings.Contains(e, "/v1/chat/completions") {
		t.Errorf("error must name the cause and the endpoint that serves the model, got %v", body["error"])
	}
}

func TestImageGeneration_RejectsMoreThanCap(t *testing.T) {
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, body := postImages(t, srv.URL, imageBody(9, imageModel)) // cap is 4
	if resp.StatusCode != 400 {
		t.Fatalf("over-cap must be a clean 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "too many images") {
		t.Errorf("error must name the cap, got %v", body["error"])
	}
}

func TestImageGeneration_BillsWhatWasDeliveredNotWhatWasAsked(t *testing.T) {
	// The worker returns ONE image for a request that asked for three. The client is
	// billed for one: the response is the evidence of what was produced.
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, bc := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, _ := postImages(t, srv.URL, imageBody(3, imageModel))
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 2) != "2.00" {
		t.Errorf("must bill the ONE delivered image ($2.00), got %s", ps.Text('f', 6))
	}
}

func TestImageGeneration_RequiresModel(t *testing.T) {
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, _ := postImages(t, srv.URL, `{"prompt":"x","n":1}`)
	if resp.StatusCode != 400 {
		t.Errorf("a missing model id must be refused, got %d", resp.StatusCode)
	}
}

func TestImageGeneration_RequiresAuth(t *testing.T) {
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/images/generations",
		strings.NewReader(imageBody(1, imageModel)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no Authorization header must be 401, got %d", resp.StatusCode)
	}
}

// The ledger row is what settlement reads later, so the image request must land in it
// with its receipt — otherwise the settlement side never sees the images at all.
func TestImageGeneration_LedgerRowCarriesReceipt(t *testing.T) {
	up := imageUpstream(t, 1, "sig-abc")
	defer up.Close()
	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()

	var logged []RequestRecord
	gw.requestLogger = newCapturingLogger(t, &logged)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	if resp, _ := postImages(t, srv.URL, imageBody(1, imageModel)); resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	readLoggedNow(t, &logged)
	if len(logged) != 1 {
		t.Fatalf("expected exactly one ledger row, got %d", len(logged))
	}
	rec := logged[0]
	if rec.Model != imageModel {
		t.Errorf("ledger model = %q, want %q", rec.Model, imageModel)
	}
	if rec.Receipt == nil {
		t.Fatal("ledger row must carry the worker receipt")
	}
	if rec.Receipt.Quantity != 1 {
		t.Errorf("receipt quantity = %d, want 1", rec.Receipt.Quantity)
	}
	if rec.TotalTokens != 0 {
		t.Errorf("an image request has no tokens, got %d", rec.TotalTokens)
	}
}

// The asset digest the gateway derives from the delivered bytes must be stable and
// order-sensitive: it is what the receipt attests and what settlement commits.
func TestCountAndHashImages_ShapesAndOrder(t *testing.T) {
	png := mockPNG(0)
	one := func() map[string]any {
		return map[string]any{"data": []map[string]string{
			{"b64_json": base64.StdEncoding.EncodeToString(png)},
		}}
	}
	b, _ := json.Marshal(one())
	n, h1 := countAndHashImages(b)
	if n != 1 || h1 == "" {
		t.Fatalf("one image must give count 1 and a hash, got %d %q", n, h1)
	}

	// Two images hash differently from one.
	two := map[string]any{"data": []map[string]string{
		{"b64_json": base64.StdEncoding.EncodeToString(png)},
		{"b64_json": base64.StdEncoding.EncodeToString(mockPNG(1))},
	}}
	b2, _ := json.Marshal(two)
	n2, h2 := countAndHashImages(b2)
	if n2 != 2 || h2 == h1 {
		t.Errorf("two images must give count 2 and a different hash, got %d %q", n2, h2)
	}

	// Order matters: swapping the two changes the digest.
	swapped := map[string]any{"data": []map[string]string{
		{"b64_json": base64.StdEncoding.EncodeToString(mockPNG(1))},
		{"b64_json": base64.StdEncoding.EncodeToString(png)},
	}}
	bs, _ := json.Marshal(swapped)
	if _, h3 := countAndHashImages(bs); h3 == h2 {
		t.Error("image order must affect the digest")
	}

	// Not the expected shape → nothing billable.
	if n, h := countAndHashImages([]byte(`{"error":"boom"}`)); n != 0 || h != "" {
		t.Errorf("a non-image response must yield (0, \"\"), got %d %q", n, h)
	}
	// An un-decodable payload is skipped rather than counted.
	bad := map[string]any{"data": []map[string]string{{"b64_json": "!!!not base64!!!"}}}
	bb, _ := json.Marshal(bad)
	if n, _ := countAndHashImages(bb); n != 0 {
		t.Errorf("an undecodable image must not be counted, got %d", n)
	}
}

func TestBase64Decode_AcceptsPaddedAndRawVariants(t *testing.T) {
	raw := mockPNG(0)
	for name, enc := range map[string]*base64.Encoding{
		"std":     base64.StdEncoding,
		"std-raw": base64.RawStdEncoding,
		"url":     base64.URLEncoding,
		"url-raw": base64.RawURLEncoding,
	} {
		got, err := base64Decode(enc.EncodeToString(raw))
		if err != nil {
			t.Errorf("%s: decode failed: %v", name, err)
			continue
		}
		if string(got) != string(raw) {
			t.Errorf("%s: decoded bytes differ", name)
		}
	}
}

// A worker that attests a digest different from the bytes it delivered must be caught.
// The signature proves it CLAIMED a digest; only recomputing it against the response
// makes the claim checkable. This is the byzantine case the asset hash exists for.
func TestImageGeneration_DetectsAssetHashMismatch(t *testing.T) {
	// The upstream returns one image but signs a receipt for a DIFFERENT digest.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"created": 1,
			"data": []map[string]string{
				{"b64_json": base64.StdEncoding.EncodeToString(mockPNG(0))},
			},
			"usage": map[string]int{"images": 1},
		})
		rcpt := map[string]any{
			"v": 1, "request_id": r.Header.Get("X-Request-ID"), "model": imageModel,
			"request_sha256": "aa", "response_sha256": "ff", "quantity": 1,
			"asset_hash": "deadbeef-not-the-delivered-bytes", "ts": 1,
			"pubkey": "pk", "sig": "sig",
		}
		b, _ := json.Marshal(rcpt)
		w.Header().Set("X-Om-Receipt", base64.StdEncoding.EncodeToString(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer up.Close()

	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()

	var logged []RequestRecord
	gw.requestLogger = newCapturingLogger(t, &logged)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	if resp, _ := postImages(t, srv.URL, imageBody(1, imageModel)); resp.StatusCode != 200 {
		t.Fatalf("expected 200 (the client is served either way), got %d", resp.StatusCode)
	}
	readLoggedNow(t, &logged)
	if len(logged) != 1 {
		t.Fatalf("expected one ledger row, got %d", len(logged))
	}
	rec := logged[0]
	if rec.Receipt == nil {
		t.Fatal("receipt must be recorded")
	}
	if rec.Receipt.Verified {
		t.Fatal("a receipt whose asset hash disagrees with the delivered bytes must NOT verify")
	}
	if !strings.Contains(rec.Receipt.VerifyError, "asset hash") {
		t.Errorf("the reason must name the asset hash, got %q", rec.Receipt.VerifyError)
	}
	// The client still got its images — verification never fails the request, it
	// records byzantine evidence.
	if rec.Status != 200 {
		t.Errorf("the request itself must still be 200, got %d", rec.Status)
	}
}

// TestImageRequestThatSwitchesModelsMarksTheWorker: a request for an image model the
// worker supports but has not loaded makes it switch. Until the switch completes the
// worker must be out of routing for that pool, or the next image requests are sent to
// the reloading engine. The chat path did this; the image path did not.
func TestImageRequestThatSwitchesModelsMarksTheWorker(t *testing.T) {
	const other = "/models/stabilityai--sdxl-turbo"
	inner := imageUpstream(t, 1, "")
	defer inner.Close()
	var gw *Gateway
	var during worker.Worker
	// Observed while the switching request is with the worker, i.e. during the switch.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cur, ok := gw.registry.Get("w0"); ok {
			during = *cur
		}
		proxyTo(t, inner.URL, w, r)
	}))
	defer up.Close()
	cfg := imageCfg()
	cfg.ModelCatalog[other] = cfg.ModelCatalog[imageModel]
	gw, _ = newImageGateway(t, up, cfg)
	defer gw.Close()
	gw.registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1,
		SupportedModels: []string{imageModel, other}})
	gw.registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, imageModel, 1)
	gw.registry.SetFeatures("w0", []string{worker.FeatureImageGen}, "")
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	if resp, body := postImages(t, srv.URL, imageBody(1, other)); resp.StatusCode != 200 {
		t.Fatalf("switching request: %d %v", resp.StatusCode, body)
	}
	if during.SwitchingTo == "" || during.State != worker.StateLoading {
		t.Fatalf("the worker must be marked switching while an image request switches it: state=%s to=%q", during.State, during.SwitchingTo)
	}
}

// proxyTo replays r against base and copies the answer back (a pass-through fake worker).
func proxyTo(t *testing.T, base string, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	req, _ := http.NewRequest(r.Method, base+r.URL.Path, bytes.NewReader(body))
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// A switch whose request completes clears its mark at once — whether the switch worked
// or failed and the worker rolled back. On a test worker a failed SDXL -> SSD-1B switch left the
// mark up and three image requests for the pool waited the 5-minute timeout.
func TestImageSwitchMarkClearsWhenTheRequestCompletes(t *testing.T) {
	const other = "/models/segmind--SSD-1B"
	for _, tc := range []struct {
		name   string
		status int
	}{{"switch failed", 500}, {"switch served", 200}} {
		t.Run(tc.name, func(t *testing.T) {
			var up *httptest.Server
			if tc.status == 200 {
				up = imageUpstream(t, 1, "")
			} else {
				up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(500)
					io.WriteString(w, `{"detail":"Model switch failed: CUDA error: out of memory"}`)
				}))
			}
			defer up.Close()
			cfg := imageCfg()
			cfg.ModelCatalog[other] = cfg.ModelCatalog[imageModel]
			gw, _ := newImageGateway(t, up, cfg)
			defer gw.Close()
			gw.registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.URL, SchedulerURL: up.URL, GPUCount: 1,
				SupportedModels: []string{imageModel, other}})
			gw.registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, imageModel, 1)
			gw.registry.SetFeatures("w0", []string{worker.FeatureImageGen}, "")
			srv := httptest.NewServer(gw.Handler())
			defer srv.Close()

			resp, _ := postImages(t, srv.URL, imageBody(1, other))
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.status)
			}
			if w, _ := gw.registry.Get("w0"); w.SwitchingTo != "" {
				t.Fatalf("the switch mark must be gone once its request completed, got switching_to=%q", w.SwitchingTo)
			}
		})
	}
}

// When the only worker for an image or speech model is mining, the 503 carries that
// worker's recovery estimate, as the chat path does — not a fixed 5 s (on a test worker a 20-minute
// WindowPoSt sent every image/speech client back every 5 s).
func TestImageAndSpeech503CarryTheRecoveryEstimate(t *testing.T) {
	up := imageUpstream(t, 1, "")
	defer up.Close()
	gw, _ := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	gw.queueTimeout = 200 * time.Millisecond
	gw.registry.UpdateState("w0", "GPU_STATE_WINDOW_POST", "paused", 0, imageModel, 1)
	gw.registry.SetUntilChange("w0", 95)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, _ := postImages(t, srv.URL, imageBody(1, imageModel))
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 while the only worker mines, got %d", resp.StatusCode)
	}
	ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	if ra < 90 || ra > 95 {
		t.Fatalf("Retry-After must be the worker's recovery estimate (~95 s), got %q", resp.Header.Get("Retry-After"))
	}
}

func TestSpeech503CarriesTheRecoveryEstimate(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, _, _ := newSpeechGateway(t, up)
	defer gw.Close()
	gw.queueTimeout = 200 * time.Millisecond
	for _, w := range gw.registry.List() {
		gw.registry.UpdateState(w.ID, "GPU_STATE_WINDOW_POST", "paused", 0, w.LoadedModel, 1)
		gw.registry.SetUntilChange(w.ID, 70)
	}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	ct, body := multipartAudio(t, asrModel, wavBytes(1.0, 16000, 1, 16))
	resp, _ := do(t, srv.URL+"/v1/audio/transcriptions", ct, body)
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 while the only worker mines, got %d", resp.StatusCode)
	}
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); ra < 65 || ra > 70 {
		t.Fatalf("Retry-After must be the worker's recovery estimate (~70 s), got %q", resp.Header.Get("Retry-After"))
	}
}
