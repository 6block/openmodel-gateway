package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"openmodel/sp-state-agent/internal/config"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

const (
	asrModel = "/models/Systran--faster-whisper-small"
	ttsModel = "/models/hexgrad--Kokoro-82M"
)

func speechCfg() *settlement.Config {
	cfg := imageCfg()
	cfg.ModelCatalog[asrModel] = settlement.ModelInfo{
		Modality: settlement.ModalityASR, PriceUnit: settlement.PriceUnitAudioSecond,
		UnitUSD: "0.01", MaxAudioSec: 30,
	}
	cfg.ModelCatalog[ttsModel] = settlement.ModelInfo{
		Modality: settlement.ModalityTTS, PriceUnit: settlement.PriceUnitCharacter,
		UnitUSD: "0.001", MaxChars: 50,
	}
	return cfg
}

// speechUpstream is a fake worker for both audio routes. It records the exact bytes and
// Content-Type it received, and signs (or misreports) a receipt as told.
type speechUpstream struct {
	srv       *httptest.Server
	gotBody   atomic.Value // []byte
	gotCT     atomic.Value // string
	wav       []byte       // TTS reply
	misreport int          // if >0, the receipt claims this quantity
	badHash   bool
	pub       ed25519.PublicKey
	priv      ed25519.PrivateKey
	// onRequest, when set, runs inside the handler — while the gateway still waits
	// for this response — so a test can observe the gateway mid-request.
	onRequest func()
}

func (u *speechUpstream) pubHex() string { return hex.EncodeToString(u.pub) }

func newSpeechUpstream(t *testing.T) *speechUpstream {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	u := &speechUpstream{wav: wavBytes(1.2, 24000, 1, 16), pub: pub, priv: priv}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.gotBody.Store(body)
		u.gotCT.Store(r.Header.Get("Content-Type"))
		if u.onRequest != nil {
			u.onRequest()
		}
		var reply []byte
		var qty int
		var asset string
		switch r.URL.Path {
		case "/v1/audio/transcriptions", "/v1/audio/translations":
			_, audio, _ := parseTranscriptionForm(body, r.Header.Get("Content-Type"))
			d, _, _ := audioDuration(audio)
			qty = billableSeconds(d)
			sum := sha256.Sum256(audio)
			asset = hex.EncodeToString(sum[:])
			reply, _ = json.Marshal(map[string]any{"text": "hello", "usage": map[string]int{"seconds": qty}})
			w.Header().Set("Content-Type", "application/json")
		case "/v1/audio/speech":
			var req struct{ Input string }
			_ = json.Unmarshal(body, &req)
			qty = settlement.CharacterCount(req.Input)
			reply = u.wav
			sum := sha256.Sum256(reply)
			asset = hex.EncodeToString(sum[:])
			w.Header().Set("Content-Type", "audio/wav")
		default:
			http.NotFound(w, r)
			return
		}
		if u.misreport > 0 {
			qty = u.misreport
		}
		if u.badHash {
			asset = strings.Repeat("0", 64)
		}
		rs := sha256.Sum256(body)
		ri := &ReceiptInfo{V: 1, RequestID: r.Header.Get("X-Request-ID"), Model: "m",
			RequestSHA256: hex.EncodeToString(rs[:]), ResponseSHA256: asset,
			Quantity: qty, AssetHash: asset, TS: 1, Pubkey: u.pubHex()}
		ri.Sig = hex.EncodeToString(ed25519.Sign(u.priv, canonicalReceiptPayload(ri)))
		b, _ := json.Marshal(ri)
		w.Header().Set("X-Om-Receipt", base64.StdEncoding.EncodeToString(b))
		w.Write(reply)
	}))
	return u
}

func newSpeechGateway(t *testing.T, up *speechUpstream, extraASR ...string) (*Gateway, *settlement.BalanceCache, *[]RequestRecord) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scfg := speechCfg()
	for _, m := range extraASR {
		scfg.ModelCatalog[m] = scfg.ModelCatalog[asrModel]
	}
	reg := worker.NewRegistry(logger, "")
	reg.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.srv.URL, SchedulerURL: up.srv.URL, GPUCount: 2})
	reg.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, asrModel, 2)
	reg.SetLoadedModels("w0", []string{asrModel, ttsModel})
	reg.SetFeatures("w0", []string{worker.FeatureReceipt, worker.FeatureASR, worker.FeatureTTS}, up.pubHex())

	pricer := settlement.NewPricer(scfg, logger)
	bc := settlement.NewBalanceCache(&fakeBalanceContract{usdc: usdcWei(1000)}, scfg.SupportedTokens, pricer, 30, logger)
	bc.SetWallets([]string{billWallet})
	bc.ForceRefresh(t.Context())
	gw := New(reg, config.GatewayConfig{
		RequestTimeoutSec: 5,
		APIKeys:           []config.APIKey{{Key: "test", Name: "user1", Wallet: billWallet}},
	}, logger)
	gw.SetBalanceChecker(bc, scfg)
	var logged []RequestRecord
	gw.requestLogger = newCapturingLogger(t, &logged)
	return gw, bc, &logged
}

func multipartAudio(t *testing.T, model string, audio []byte) (string, []byte) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("model", model)
	fw, _ := mw.CreateFormFile("file", "clip.wav")
	fw.Write(audio)
	mw.Close()
	return mw.FormDataContentType(), b.Bytes()
}

func do(t *testing.T, url, ct string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", ct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

// TestTranscription_BillsCeilSecondsMeasuredByTheGateway: 3.4 s of audio bills 4
// seconds at $0.01, the multipart body reaches the worker byte-for-byte with its own
// boundary, and the ledger row carries the gateway's own count and the upload digest.
func TestTranscription_BillsCeilSecondsMeasuredByTheGateway(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, bc, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	audio := wavBytes(3.4, 16000, 1, 16)
	ct, body := multipartAudio(t, asrModel, audio)
	resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if got := up.gotBody.Load().([]byte); !bytes.Equal(got, body) {
		t.Fatal("the multipart body must reach the worker verbatim (the receipt hashes it)")
	}
	if got := up.gotCT.Load().(string); got != ct {
		t.Fatalf("Content-Type (with boundary) must pass through, got %q want %q", got, ct)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 2) != "0.04" {
		t.Errorf("pending must be $0.04 (4 s × $0.01), got %s", ps.Text('f', 6))
	}
	readLoggedNow(t, logged)
	rec := (*logged)[0]
	sum := sha256.Sum256(audio)
	if rec.Quantity != 4 || rec.AssetHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("ledger quantity/hash = %d/%s; want 4 and the upload digest", rec.Quantity, rec.AssetHash)
	}
	if rec.Receipt == nil || !rec.Receipt.Verified {
		t.Fatalf("an agreeing receipt must verify, got %+v", rec.Receipt)
	}
}

// TestSpeech_BillsCharactersAndHashesTheReply: characters are code points (emoji = 1),
// the asset digest is of the WAV the client received.
func TestSpeech_BillsCharactersAndHashesTheReply(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, bc, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	input := "你好 world 😀"
	body, _ := json.Marshal(map[string]string{"model": ttsModel, "input": input})
	resp, raw := do(t, srv.URL+"/v1/audio/speech", "application/json", body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !bytes.Equal(raw, up.wav) {
		t.Fatal("the client must receive the worker's audio unchanged")
	}
	n := settlement.CharacterCount(input)
	if n != 10 { // 你 好 ␠ w o r l d ␠ 😀
		t.Fatalf("test premise: %q is 10 code points, got %d", input, n)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 3) != "0.010" {
		t.Errorf("pending must be $0.010 (10 chars × $0.001), got %s", ps.Text('f', 6))
	}
	readLoggedNow(t, logged)
	sum := sha256.Sum256(up.wav)
	if rec := (*logged)[0]; rec.Quantity != n || rec.AssetHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("ledger = %d/%s, want %d and the reply digest", rec.Quantity, rec.AssetHash, n)
	}
}

// TestSpeech_MisreportingWorkerIsCaught: a receipt claiming more units than the gateway
// measured is invalid, and the bill stays at the measured amount.
func TestSpeech_MisreportingWorkerIsCaught(t *testing.T) {
	up := newSpeechUpstream(t)
	up.misreport = 40
	defer up.srv.Close()
	gw, bc, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	ct, body := multipartAudio(t, asrModel, wavBytes(2.0, 16000, 1, 16))
	if resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 2) != "0.02" {
		t.Errorf("bill must stay at the measured 2 s, got %s", ps.Text('f', 6))
	}
	readLoggedNow(t, logged)
	if r := (*logged)[0].Receipt; r == nil || r.Verified {
		t.Fatalf("an inflated receipt must be marked invalid, got %+v", r)
	}
}

func TestSpeech_AssetHashMismatchIsByzantineEvidence(t *testing.T) {
	up := newSpeechUpstream(t)
	up.badHash = true
	defer up.srv.Close()
	gw, _, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	body, _ := json.Marshal(map[string]string{"model": ttsModel, "input": "hello"})
	do(t, srv.URL+"/v1/audio/speech", "application/json", body)
	readLoggedNow(t, logged)
	if r := (*logged)[0].Receipt; r == nil || r.Verified || !strings.Contains(r.VerifyError, "asset hash") {
		t.Fatalf("a mismatched audio digest must invalidate the receipt, got %+v", r)
	}
}

func TestAudio_Refusals(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, bc, _ := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	mp3 := append([]byte("ID3\x04\x00"), make([]byte, 200)...)
	ct, body := multipartAudio(t, asrModel, mp3)
	if resp, _ := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 400 {
		t.Errorf("unmeasurable audio must be 400, got %d", resp.StatusCode)
	}
	ct, body = multipartAudio(t, asrModel, wavBytes(31, 16000, 1, 16))
	if resp, _ := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 400 {
		t.Errorf("audio over max_audio_sec must be 400, got %d", resp.StatusCode)
	}
	ct, body = multipartAudio(t, ttsModel, wavBytes(1, 16000, 1, 16))
	if resp, _ := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 400 {
		t.Errorf("a TTS model on the transcription route must be 400, got %d", resp.StatusCode)
	}
	long, _ := json.Marshal(map[string]string{"model": ttsModel, "input": strings.Repeat("a", 51)})
	if resp, _ := do(t, srv.URL+"/v1/audio/speech", "application/json", long); resp.StatusCode != 400 {
		t.Errorf("input over max_chars must be 400, got %d", resp.StatusCode)
	}
	oggreq, _ := json.Marshal(map[string]string{"model": ttsModel, "input": "hi", "response_format": "ogg"})
	if resp, _ := do(t, srv.URL+"/v1/audio/speech", "application/json", oggreq); resp.StatusCode != 400 {
		t.Errorf("an unknown response_format must be 400, got %d", resp.StatusCode)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("refused requests must leave nothing reserved, got %s", ps.Text('f', 6))
	}
}

// TestForward_RefusesOversizedReplyInsteadOfTruncating: a reply past the cap is an
// error. Truncating would bill and hash a cut-off file as if it were whole.
func TestForward_RefusesOversizedReplyInsteadOfTruncating(t *testing.T) {
	up := newSpeechUpstream(t)
	up.wav = make([]byte, 4096)
	defer up.srv.Close()
	gw, bc, _ := newSpeechGateway(t, up)
	gw.maxResponseBytes = 1024
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	body, _ := json.Marshal(map[string]string{"model": ttsModel, "input": "hi"})
	if resp, _ := do(t, srv.URL+"/v1/audio/speech", "application/json", body); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an oversized reply must be 502, got %d", resp.StatusCode)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Sign() != 0 {
		t.Errorf("a failed request must not be billed, got %s", ps.Text('f', 6))
	}
}

// TestSpeechRequestThatSwitchesModelsMarksThePool: an ASR request for a supported but
// not-resident speech model makes the worker's ASR pool switch. Its pool's models must
// be out of routing until the switch completes; the worker's other pools stay routed.
func TestSpeechRequestThatSwitchesModelsMarksThePool(t *testing.T) {
	const medium = "/models/Systran--faster-whisper-medium"
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, _, _ := newSpeechGateway(t, up, medium)
	defer gw.Close()
	gw.registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.srv.URL, SchedulerURL: up.srv.URL, GPUCount: 2,
		SupportedModels: []string{asrModel, ttsModel, medium}})
	gw.registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, asrModel, 2)
	gw.registry.SetLoadedModels("w0", []string{asrModel, ttsModel})
	gw.registry.SetPoolGroups("w0", [][]string{{asrModel, medium}, {ttsModel}})
	gw.registry.SetFeatures("w0", []string{worker.FeatureASR, worker.FeatureTTS}, up.pubHex())
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	// Observed while the switching request is with the worker, i.e. during the switch.
	var asrRoutable, ttsRoutable error
	up.onRequest = func() {
		_, asrRoutable = selectWorkerForModel(gw.registry, medium, nil)
		_, ttsRoutable = selectWorkerForModel(gw.registry, ttsModel, nil)
	}
	ct, body := multipartAudio(t, medium, wavBytes(1.0, 16000, 1, 16))
	if resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 200 {
		t.Fatalf("switching request: %d %s", resp.StatusCode, raw)
	}
	if asrRoutable == nil {
		t.Fatal("the ASR pool is switching: further ASR requests must wait")
	}
	if ttsRoutable != nil {
		t.Fatalf("the TTS pool is unaffected and must stay routable: %v", ttsRoutable)
	}
}

// The speech entry points clear their switch mark when the request completes, as the
// image and chat ones do; otherwise the pool waits out the 5-minute timeout after a
// switch that failed or was replaced before a poll saw it.
func TestSpeechSwitchMarkClearsWhenTheRequestCompletes(t *testing.T) {
	const medium = "/models/Systran--faster-whisper-medium"
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, _, _ := newSpeechGateway(t, up, medium)
	defer gw.Close()
	gw.registry.Register(worker.WorkerRegistration{ID: "w0", Endpoint: up.srv.URL, SchedulerURL: up.srv.URL, GPUCount: 2,
		SupportedModels: []string{asrModel, ttsModel, medium}})
	gw.registry.UpdateState("w0", "GPU_STATE_AVAILABLE", "running", 0, asrModel, 2)
	gw.registry.SetLoadedModels("w0", []string{asrModel, ttsModel})
	gw.registry.SetPoolGroups("w0", [][]string{{asrModel, medium}, {ttsModel}})
	gw.registry.SetFeatures("w0", []string{worker.FeatureASR, worker.FeatureTTS}, up.pubHex())
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	ct, body := multipartAudio(t, medium, wavBytes(1.0, 16000, 1, 16))
	if resp, raw := do(t, srv.URL+"/v1/audio/transcriptions", ct, body); resp.StatusCode != 200 {
		t.Fatalf("switching request: %d %s", resp.StatusCode, raw)
	}
	if w, _ := gw.registry.Get("w0"); w.SwitchingTo != "" {
		t.Fatalf("the switch mark must be gone once its request completed, got %q", w.SwitchingTo)
	}
}
