package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// audio.go — /v1/audio/transcriptions and /v1/audio/translations (ASR) and
// /v1/audio/speech (TTS).
//
// BOTH QUANTITIES ARE MEASURED HERE, never taken from the worker:
//
//	transcription  ceil(duration of the UPLOADED audio), minimum 1 second — read from the
//	               headers of the bytes the client sent (audioduration.go); translation
//	               bills the same way
//	speech         Unicode code points of the request's `input` text
//
// and both asset digests are of bytes this process handled: the uploaded file for ASR,
// the returned audio for TTS. The worker's signed receipt must agree with both, or it is
// marked invalid (byzantine evidence) — the same rule the image path applies.
//
// The request is forwarded VERBATIM (multipart included, boundary and all) because the
// receipt attests sha256 of exactly the bytes the worker received.

// assetRequest is what the two handlers hand to the shared pipeline.
type assetRequest struct {
	model       string
	unit        settlement.PriceUnit
	quantity    int    // billable quantity, measured by the gateway (TTS) or 0 if measured from the reply
	assetHash   string // known before forwarding (ASR: the upload); "" if it is the reply's
	body        []byte
	contentType string
	kind        string // "transcription" | "speech", for logs and errors
	feature     string // worker feature the target must advertise
	endpoint    endpointKind
}

func (g *Gateway) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	g.serveAssetRequest(w, r, g.maxAudioUploadBytes(), func(body []byte, ct string) (*assetRequest, int, string) {
		model, audio, err := parseTranscriptionForm(body, ct)
		if err != nil {
			return nil, http.StatusBadRequest, err.Error()
		}
		dur, _, err := audioDuration(audio)
		if err != nil {
			return nil, http.StatusBadRequest, err.Error()
		}
		sum := sha256.Sum256(audio)
		return &assetRequest{
			model: model, unit: settlement.PriceUnitAudioSecond,
			quantity: billableSeconds(dur), assetHash: hex.EncodeToString(sum[:]),
			body: body, contentType: ct, kind: "transcription", feature: worker.FeatureASR,
			endpoint: endpointTranscription,
		}, 0, ""
	})
}

func (g *Gateway) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	g.serveAssetRequest(w, r, 0, func(body []byte, ct string) (*assetRequest, int, string) {
		if !json.Valid(body) {
			return nil, http.StatusBadRequest, "request body is not valid JSON"
		}
		var req struct {
			Model          string `json:"model"`
			Input          string `json:"input"`
			ResponseFormat string `json:"response_format"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, http.StatusBadRequest, "request body is not a valid speech request"
		}
		if strings.TrimSpace(req.Input) == "" {
			return nil, http.StatusBadRequest, "input text is required"
		}
		if f := strings.ToLower(req.ResponseFormat); f != "" && !speechFormats[f] {
			// The gateway hashes and returns exactly the bytes the worker produced, in
			// whichever of these formats was asked for.
			return nil, http.StatusBadRequest, "response_format must be wav, mp3, opus, aac, flac or pcm"
		}
		return &assetRequest{
			model: req.Model, unit: settlement.PriceUnitCharacter,
			quantity: settlement.CharacterCount(req.Input),
			body:     body, contentType: "application/json", kind: "speech", feature: worker.FeatureTTS,
			endpoint: endpointSpeech,
		}, 0, ""
	})
}

// speechFormats are the /v1/audio/speech response formats workers produce.
var speechFormats = map[string]bool{"wav": true, "mp3": true, "opus": true, "aac": true, "flac": true, "pcm": true}

func (g *Gateway) maxAudioUploadBytes() int64 {
	if g.maxAudioUpload > 0 {
		return g.maxAudioUpload
	}
	return defaultMaxAudioUploadBytes
}

// parseTranscriptionForm extracts the model name and the audio file from a multipart
// body WITHOUT altering the body (it is forwarded verbatim afterwards).
func parseTranscriptionForm(body []byte, contentType string) (string, []byte, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return "", nil, fmt.Errorf("expected multipart/form-data with an audio file")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var model string
	var audio []byte
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("malformed multipart body: %v", err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return "", nil, fmt.Errorf("malformed multipart body: %v", err)
		}
		switch p.FormName() {
		case "model":
			model = strings.TrimSpace(string(data))
		case "file":
			audio = data
		}
	}
	if len(audio) == 0 {
		return "", nil, fmt.Errorf(`missing audio file field "file"`)
	}
	return model, audio, nil
}

// serveAssetRequest is the shared pipeline for asset-billed requests whose quantity the
// gateway measures from the request (or, if quantity is 0 at parse time, from nothing —
// not used by these handlers). parse returns a request or an HTTP status + message.
func (g *Gateway) serveAssetRequest(w http.ResponseWriter, r *http.Request, bodyCap int64,
	parse func(body []byte, contentType string) (*assetRequest, int, string)) {

	if g.draining.Load() {
		w.Header().Set("Retry-After", "5")
		jsonError(w, "server is shutting down, please retry", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)

	requestID := generateRequestID()
	w.Header().Set("X-Request-ID", requestID)
	defer g.registry.EndSwitchBy(requestID)

	keyInfo, ok := g.authenticate(r)
	if !ok {
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}
	if g.rateLimiter != nil {
		if !g.rateLimiter.AllowRate(keyInfo.Name) {
			metrics.RateLimitRejectedTotal.WithLabelValues("rate").Inc()
			w.Header().Set("Retry-After", "1")
			jsonError(w, "rate limit exceeded for this API key", http.StatusTooManyRequests)
			return
		}
		release, ok := g.rateLimiter.Acquire(keyInfo.Name)
		if !ok {
			metrics.RateLimitRejectedTotal.WithLabelValues("concurrency").Inc()
			w.Header().Set("Retry-After", "1")
			jsonError(w, "too many concurrent requests for this API key", http.StatusTooManyRequests)
			return
		}
		defer release()
	}

	body, ok := g.readBodyCapped(w, r, keyInfo.Name, requestID, bodyCap)
	if !ok {
		return
	}
	req, code, msg := parse(body, r.Header.Get("Content-Type"))
	if req == nil {
		metrics.GatewayRequestsTotal.WithLabelValues(fmt.Sprintf("%d", code), "none").Inc()
		jsonError(w, msg, code)
		return
	}
	if m := strings.TrimSpace(req.model); m == "" || m == "default" || m == "unknown" {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "a specific model id is required; call GET /v1/models to list available models", http.StatusBadRequest)
		return
	}
	req.model = g.canonicalCatalogModel(strings.TrimSpace(req.model))
	if !g.checkEndpointKind(w, req.endpoint, req.model) {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		return
	}
	if g.unknownModel404(w, req.model) {
		metrics.GatewayRequestsTotal.WithLabelValues("404", "none").Inc()
		return
	}

	// The model's CONFIGURED unit must be the one this endpoint bills in. A worker or a
	// client cannot pick the unit: a text model is not transcription-billable, and a
	// per-second model is not character-billable.
	var unitPrice *big.Float
	if g.settlementCfg != nil {
		if got := g.settlementCfg.UnitFor(req.model); got != req.unit {
			metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
			jsonError(w, fmt.Sprintf("model %q is not a %s model", req.model, req.kind), http.StatusBadRequest)
			return
		}
		if info, found := g.settlementCfg.ModelCatalog[req.model]; found {
			limit := info.MaxAudioSec
			if req.unit == settlement.PriceUnitCharacter {
				limit = info.MaxChars
			}
			if limit > 0 && req.quantity > limit {
				metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
				jsonError(w, fmt.Sprintf("%s too large: %d %s, at most %d", req.kind, req.quantity, req.unit, limit), http.StatusBadRequest)
				return
			}
		}
		unitPrice = g.prices.UnitPrice(req.model)
		if unitPrice == nil {
			metrics.GatewayRequestsTotal.WithLabelValues("500", "none").Inc()
			jsonError(w, "model is misconfigured: no per-unit price", http.StatusInternalServerError)
			return
		}
	}

	// The quantity is known before forwarding, so the reservation is EXACT. It is
	// released in full unless the worker delivers (settledQty set below).
	settledQty := 0
	if g.balanceChecker != nil && keyInfo.Wallet != "" && unitPrice != nil {
		if suspended, debt := g.balanceChecker.IsSuspended(keyInfo.Wallet); suspended {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			g.logger.Warn("request refused: wallet suspended for unpaid debt",
				"api_key", keyInfo.Name, "wallet", keyInfo.Wallet, "debt_usd", debt.Text('f', 6), "request_id", requestID)
			jsonError(w, "account suspended: outstanding debt must be settled (top up your balance and wait for the next settlement cycle)", http.StatusPaymentRequired)
			return
		}
		est := new(big.Float).Mul(unitPrice, big.NewFloat(float64(req.quantity)))
		if !g.balanceChecker.Warm() {
			// Balances are still loading after a restart; "insufficient" would be a lie.
			metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
			w.Header().Set("Retry-After", "5")
			jsonError(w, "gateway is starting (loading balances), please retry", http.StatusServiceUnavailable)
			return
		}
		if !g.requestLogger.Writable() {
			// A record that cannot be written is never billed: refuse rather than
			// serve for free until the ledger disk has room again.
			metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
			g.logger.Error("request refused: request ledger is not writable (disk full?)", "request_id", requestID)
			w.Header().Set("Retry-After", "30")
			jsonError(w, "gateway cannot record usage right now, please retry later", http.StatusServiceUnavailable)
			return
		}
		if !g.balanceChecker.Reserve(keyInfo.Wallet, est) {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			jsonError(w, "insufficient balance", http.StatusPaymentRequired)
			return
		}
		defer func() { g.adjustBalance(keyInfo.Wallet, est, req.model, tokenUsage{}, settledQty) }()
	}

	target, err := g.selectWithAffinity("", req.model)
	if err != nil {
		target, err = g.blockForWorker(r.Context(), req.model, time.Now().Add(g.queueWindowFor(req.model)))
		if err != nil {
			metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
			// The recovery estimate of the workers that serve this model, as the chat
			// path gives: a fixed 5 s sent clients back every 5 s through a 20-minute
			// WindowPoSt of the only worker holding the model.
			w.Header().Set("Retry-After", g.retryAfterFor(req.model))
			jsonError(w, "no worker available for this model", http.StatusServiceUnavailable)
			return
		}
	}
	g.noteSwitch(target, req.model, requestID)
	// Speech bodies are forwarded byte-for-byte (the receipt hashes them), so unlike the
	// JSON paths the model field is NOT rewritten to the worker's spelling; the worker
	// accepts either spelling for a resident model.

	t0 := time.Now()
	var respBody []byte
	var status int
	var upstreamHeader http.Header
	var workerID string
	var sent []byte
	respBody, status, upstreamHeader, workerID, target, sent, err = g.forwardWithRetry(
		r, req.model, req.body, req.contentType, target, requestID)
	if err != nil {
		g.logger.Error(req.kind+" proxy error", "request_id", requestID, "worker", workerID, "error", err)
		metrics.GatewayRequestsTotal.WithLabelValues("502", workerID).Inc()
		jsonError(w, "upstream worker unreachable or reply too large", http.StatusBadGateway)
		return
	}
	elapsed := time.Since(t0)

	assetHash := req.assetHash
	quantity := 0
	if status == http.StatusOK {
		if assetHash == "" { // TTS: the asset is the delivered audio
			sum := sha256.Sum256(respBody)
			assetHash = hex.EncodeToString(sum[:])
		}
		quantity = req.quantity
	}

	var receipt *ReceiptInfo
	if status == http.StatusOK && target.HasFeature(worker.FeatureReceipt) {
		receipt = g.captureReceipt(upstreamHeader.Get("X-Om-Receipt"), workerID,
			target.ReceiptPubkey, sent, tokenUsage{AssetQty: quantity}, true, requestID)
		if receipt != nil && receipt.AssetHash != "" && receipt.AssetHash != assetHash {
			receipt.Verified = false
			receipt.VerifyError = "attested asset hash differs from the bytes the gateway handled"
			metrics.ReceiptsTotal.WithLabelValues("invalid").Inc()
			g.logger.Error(req.kind+" receipt asset hash mismatch (byzantine evidence)",
				"request_id", requestID, "worker", workerID, "attested", receipt.AssetHash, "measured", assetHash)
		}
	}
	settledQty = quantity

	metrics.GatewayRequestsTotal.WithLabelValues(fmt.Sprintf("%d", status), workerID).Inc()
	metrics.GatewayRequestDuration.WithLabelValues(workerID).Observe(elapsed.Seconds())
	g.logger.Info("gateway "+req.kind+" request", "request_id", requestID, "api_key", keyInfo.Name,
		"worker", workerID, "model", req.model, "status", status, "unit", string(req.unit),
		"quantity", quantity, "duration_ms", elapsed.Milliseconds())

	rec := RequestRecord{
		RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
		Wallet: keyInfo.Wallet, WorkerID: workerID, Path: r.URL.Path,
		Model: req.model, Status: status, DurationMs: elapsed.Milliseconds(), Receipt: receipt,
	}
	if quantity > 0 {
		rec.Quantity, rec.AssetHash = quantity, assetHash
	}
	g.logRequest(rec)

	copyResponseHeaders(w.Header(), upstreamHeader)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("X-Request-ID", requestID)
	respBody = normalizeErrorBody(status, respBody, w.Header())
	w.WriteHeader(status)
	w.Write(respBody)
}

// readBodyCapped is readBody with an explicit cap (0 = the general request cap).
func (g *Gateway) readBodyCapped(w http.ResponseWriter, r *http.Request, keyName, requestID string, bodyCap int64) ([]byte, bool) {
	if bodyCap <= 0 {
		return g.readBody(w, r, keyName, requestID)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyCap+1))
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return nil, false
	}
	if int64(len(body)) > bodyCap {
		metrics.RateLimitRejectedTotal.WithLabelValues("body_too_large").Inc()
		jsonError(w, fmt.Sprintf("request body exceeds limit of %d bytes", bodyCap), http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}
