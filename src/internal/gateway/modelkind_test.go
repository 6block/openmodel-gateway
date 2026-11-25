package gateway

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

const textModel = "/models/Qwen--Qwen3-32B-AWQ"

// unifiedFleet is one worker serving all four kinds, behind one gateway, the shape of
// a multi-pool SP. It counts upstream calls: a refused request must never reach it.
func unifiedFleet(t *testing.T) (*httptest.Server, *Gateway, *settlement.BalanceCache, *atomic.Int32) {
	t.Helper()
	up := newSpeechUpstream(t)
	var calls atomic.Int32
	inner := up.srv.Config.Handler
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			return
		}
		inner.ServeHTTP(w, r)
	})
	t.Cleanup(up.srv.Close)
	gw, bc, _ := newSpeechGateway(t, up)
	gw.settlementCfg.ModelCatalog[textModel] = settlement.ModelInfo{InputUSD: "0.60", CacheReadUSD: "0.15", ContextWindow: 8192, MaxOutput: 4096}
	gw.settlementCfg.ModelPricesUSD[textModel] = "0.60"
	gw.prices = settlement.NewPrices(gw.settlementCfg)
	gw.registry.SetLoadedModels("w0", []string{asrModel, ttsModel, textModel})
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(func() { srv.Close(); gw.Close() })
	return srv, gw, bc, &calls
}

func errMessage(raw []byte) string {
	var e struct{ Error struct{ Message string } }
	_ = json.Unmarshal(raw, &e)
	return e.Error.Message
}

// TestEveryEndpointRefusesTheWrongKindOfModel: each endpoint serves one family; a model
// of another family is a 400 naming the endpoint that does serve it — and the request
// never reaches a worker. Before: a chat request for a speech model came back as a
// worker 500/502.
func TestEveryEndpointRefusesTheWrongKindOfModel(t *testing.T) {
	srv, _, bc, calls := unifiedFleet(t)
	chat := func(m string) []byte {
		b, _ := json.Marshal(map[string]any{"model": m, "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 5})
		return b
	}
	speech := func(m string) []byte { b, _ := json.Marshal(map[string]string{"model": m, "input": "hello"}); return b }
	cases := []struct {
		name, path, model, ct, wantHint string
		body                            []byte
	}{
		{"chat ← asr", "/v1/chat/completions", asrModel, "application/json", "/v1/audio/transcriptions", chat(asrModel)},
		{"chat ← tts", "/v1/chat/completions", ttsModel, "application/json", "/v1/audio/speech", chat(ttsModel)},
		{"completions ← tts", "/v1/completions", ttsModel, "application/json", "/v1/audio/speech", chat(ttsModel)},
		{"speech ← text", "/v1/audio/speech", textModel, "application/json", "/v1/chat/completions", speech(textModel)},
		{"speech ← asr", "/v1/audio/speech", asrModel, "application/json", "/v1/audio/transcriptions", speech(asrModel)},
	}
	ct, mp := multipartAudio(t, ttsModel, wavBytes(1, 16000, 1, 16))
	cases = append(cases, struct {
		name, path, model, ct, wantHint string
		body                            []byte
	}{"transcription ← tts", "/v1/audio/transcriptions", ttsModel, ct, "/v1/audio/speech", mp})
	for _, c := range cases {
		resp, raw := do(t, srv.URL+c.path, c.ct, c.body)
		if resp.StatusCode != 400 || !strings.Contains(errMessage(raw), c.wantHint) {
			t.Errorf("%s: %d %q — want 400 naming %s", c.name, resp.StatusCode, errMessage(raw), c.wantHint)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("refused requests must not reach a worker, %d did", n)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("refused requests must reserve nothing, got %s", ps.Text('f', 6))
	}
}

// TestListedIDsWorkOnEveryEndpoint: /v1/models lists the HuggingFace spelling
// ("hexgrad/Kokoro-82M"); the catalog is keyed by the weight path. Before, a speech or
// transcription request using the LISTED id was refused as "not a speech model", and a
// text request was billed at the default price instead of the model's own.
func TestListedIDsWorkOnEveryEndpoint(t *testing.T) {
	srv, gw, bc, _ := unifiedFleet(t)
	gw.settlementCfg.ModelPricesUSD["default"] = "100" // a default far from the model's own $0.60
	gw.prices = settlement.NewPrices(gw.settlementCfg)

	b, _ := json.Marshal(map[string]string{"model": "hexgrad/Kokoro-82M", "input": "hello"})
	if resp, raw := do(t, srv.URL+"/v1/audio/speech", "application/json", b); resp.StatusCode != 200 {
		t.Fatalf("speech by listed id: %d %s", resp.StatusCode, raw)
	}
	ct, mp := multipartAudio(t, "Systran/faster-whisper-small", wavBytes(2.0, 16000, 1, 16))
	if resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, mp); resp.StatusCode != 200 {
		t.Fatalf("transcription by listed id: %d %s", resp.StatusCode, raw)
	}
	speechAndASR := bc.GetPendingSpend(billWallet).Text('f', 6) // 5 chars × $0.001 + 2 s × $0.01
	if speechAndASR != "0.025000" {
		t.Fatalf("speech+asr by listed id must bill $0.025, got %s", speechAndASR)
	}

	b, _ = json.Marshal(map[string]any{"model": "Qwen/Qwen3-32B-AWQ", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 5})
	if resp, raw := do(t, srv.URL+"/v1/chat/completions", "application/json", b); resp.StatusCode != 200 {
		t.Fatalf("chat by listed id: %d %s", resp.StatusCode, raw)
	}
	// 10 prompt × $0.60/M + 5 completion × $0.60/M = $0.000009, not 15 × $100/M = $0.0015.
	base, _ := new(big.Float).SetString("0.025")
	got := new(big.Float).Sub(bc.GetPendingSpend(billWallet), base).Text('f', 6)
	if got != "0.000009" {
		t.Fatalf("chat by listed id must bill the model's own price ($0.000009), got %s", got)
	}
}

func TestUpstreamErrorsAreNormalized(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"detail":"too many images: 3 requested, at most 2"}`, "too many images: 3 requested, at most 2"},
		{422, `{"detail":[{"loc":["body","input"],"msg":"field required"}]}`, `field required`},
		{500, `Internal Server Error`, "Internal Server Error"},
		{502, ``, "Bad Gateway"},
		{503, `{"error":{"message":"GPU yielded to mining","type":"service_unavailable"}}`, "GPU yielded to mining"},
	} {
		h := http.Header{}
		out := normalizeErrorBody(c.status, []byte(c.body), h)
		var e struct {
			Error struct{ Message, Type string }
		}
		if err := json.Unmarshal(out, &e); err != nil || !strings.Contains(e.Error.Message, c.want) || e.Error.Type == "" {
			t.Errorf("%d %q → %s, want message containing %q", c.status, c.body, out, c.want)
		}
	}
	ok := []byte(`{"data":[]}`)
	if got := normalizeErrorBody(200, ok, http.Header{}); string(got) != string(ok) {
		t.Errorf("a 2xx body must pass through unchanged")
	}
}

func TestModelsListingNamesKindAndEndpoint(t *testing.T) {
	srv, _, _, _ := unifiedFleet(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	type entry struct {
		ID        string `json:"id"`
		Modality  string `json:"modality"`
		PriceUnit string `json:"price_unit"`
		Endpoint  string `json:"endpoint"`
	}
	var out struct {
		Data []entry `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	want := map[string][3]string{
		"Qwen/Qwen3-32B-AWQ":           {"text", "token", "/v1/chat/completions"},
		"Systran/faster-whisper-small": {"asr", "audio_second", "/v1/audio/transcriptions"},
		"hexgrad/Kokoro-82M":           {"tts", "character", "/v1/audio/speech"},
	}
	seen := 0
	for _, m := range out.Data {
		if w, ok := want[m.ID]; ok {
			seen++
			if got := [3]string{m.Modality, m.PriceUnit, m.Endpoint}; got != w {
				t.Errorf("%s: %v, want %v", m.ID, got, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("listing %+v is missing models", out.Data)
	}
}

func TestCatalogShowsNoTokenPriceForPerUnitModels(t *testing.T) {
	srv, _, _, _ := unifiedFleet(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/catalog", nil)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	for _, m := range out.Data {
		perUnit := m["price_unit"] != "token"
		in := m["input_price_usd_per_1m"]
		if perUnit && (in != "" || m["unit_price_usd"] == nil) {
			t.Errorf("%v: per-unit model must show only its unit price, got input=%v unit=%v", m["id"], in, m["unit_price_usd"])
		}
		if !perUnit && m["id"] == textModel && in != "0.60" {
			t.Errorf("text model keeps its token price, got %v", in)
		}
	}
}

func TestUnknownModelIs404OnEveryEndpoint(t *testing.T) {
	srv, _, _, calls := unifiedFleet(t)
	chat, _ := json.Marshal(map[string]any{"model": "no-such-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	speech, _ := json.Marshal(map[string]string{"model": "no-such-model", "input": "hi"})
	img, _ := json.Marshal(map[string]string{"model": "no-such-model", "prompt": "x"})
	ct, mp := multipartAudio(t, "no-such-model", wavBytes(1, 16000, 1, 16))
	for _, c := range []struct {
		path, ct string
		body     []byte
	}{
		{"/v1/chat/completions", "application/json", chat},
		{"/v1/audio/speech", "application/json", speech},
		{"/v1/images/generations", "application/json", img},
		{"/v1/audio/transcriptions", ct, mp},
	} {
		resp, raw := do(t, srv.URL+c.path, c.ct, c.body)
		if resp.StatusCode != 404 || !strings.Contains(errMessage(raw), "no worker supports model") {
			t.Errorf("%s: %d %q, want 404", c.path, resp.StatusCode, errMessage(raw))
		}
	}
	if calls.Load() != 0 {
		t.Errorf("an unknown model must not reach a worker")
	}
}

// TestPerUnitEndpointsRetryOnAnotherWorker: image and audio requests whose first worker
// could not serve them (503 mining, 502 engine down) are retried on another worker and
// billed once; the receipt is checked against the bytes the serving worker received.
func TestPerUnitEndpointsRetryOnAnotherWorker(t *testing.T) {
	for _, code := range []int{502, 503} {
		good := newSpeechUpstream(t)
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		gw, bc, logged := newSpeechGateway(t, good)
		gw.registry.Register(worker.WorkerRegistration{ID: "bad", Endpoint: bad.URL, SchedulerURL: bad.URL, GPUCount: 100})
		gw.registry.UpdateState("bad", "GPU_STATE_AVAILABLE", "running", 0, ttsModel, 100)
		gw.registry.SetLoadedModels("bad", []string{asrModel, ttsModel})
		gw.registry.SetFeatures("bad", []string{worker.FeatureReceipt, worker.FeatureASR, worker.FeatureTTS}, good.pubHex())
		srv := httptest.NewServer(gw.Handler())

		b, _ := json.Marshal(map[string]string{"model": ttsModel, "input": "hello"})
		ok := 0
		for i := 0; i < 6; i++ { // weight 100:2 → the bad worker is picked first nearly every time
			if resp, raw := do(t, srv.URL+"/v1/audio/speech", "application/json", b); resp.StatusCode == 200 {
				ok++
			} else {
				t.Errorf("%d: speech got %d %s", code, resp.StatusCode, raw)
			}
		}
		ct, mp := multipartAudio(t, asrModel, wavBytes(2, 16000, 1, 16))
		if resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, mp); resp.StatusCode != 200 {
			t.Errorf("%d: transcription got %d %s", code, resp.StatusCode, raw)
		}
		if ps := bc.GetPendingSpend(billWallet).Text('f', 3); ps != "0.050" { // 6×5 chars×$0.001 + 2 s×$0.01
			t.Errorf("%d: billed %s, want 0.050 (each request once)", code, ps)
		}
		readLoggedNow(t, logged)
		for _, rec := range *logged {
			if rec.Status == 200 && (rec.WorkerID != "w0" || rec.Receipt == nil || !rec.Receipt.Verified) {
				t.Errorf("%d: served by %s receipt %+v; want w0 with a verified receipt", code, rec.WorkerID, rec.Receipt)
			}
		}
		srv.Close()
		gw.Close()
		bad.Close()
		good.srv.Close()
	}
}
