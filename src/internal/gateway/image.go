package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"openmodel/sp-state-agent/internal/metrics"
	"openmodel/sp-state-agent/internal/worker"
)

// image.go — the image-generation endpoint.
//
// WHY A SEPARATE HANDLER rather than a branch inside handleProxy: the chat path is built
// around streaming and token metering — prompt-token estimation, SSE capture, session
// affinity on the prompt prefix. None of that applies here: image generation is one
// non-streaming call whose billable amount is a count of images. Sharing the handler
// would mean threading "is this an image request" through each of those paths; a second
// handler reuses what genuinely is shared (auth, routing, the balance gate, the ledger,
// receipts) and leaves the rest untouched.
//
// WHAT IS SHARED and must therefore behave identically: the balance reservation and its
// deferred adjustment, the ledger row, and receipt capture + verification. The UNIT
// differs; the discipline does not.

type imageGenerationRequest struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	N              int      `json:"n"`
	Size           string   `json:"size"`
	Steps          int      `json:"steps"`
	Guidance       *float64 `json:"guidance,omitempty"`
	Seed           *int64   `json:"seed,omitempty"`
	ResponseFormat string   `json:"response_format"`
}

func (g *Gateway) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if g.draining.Load() {
		w.Header().Set("Retry-After", "5")
		jsonError(w, "server is shutting down, please retry", http.StatusServiceUnavailable)
		return
	}
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)

	requestID := generateRequestID()
	w.Header().Set("X-Request-ID", requestID)
	defer g.registry.EndSwitchBy(requestID)

	keyInfo, ok := g.authenticate(r)
	if !ok {
		g.logger.Warn("auth failed", "path", r.URL.Path, "remote", r.RemoteAddr)
		jsonError(w, "invalid or missing Authorization header", http.StatusUnauthorized)
		return
	}

	// Same per-key abuse controls as the chat path: a rate limit first (cheap, no slot
	// held), then a concurrency slot for the duration of the request.
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

	body, ok := g.readBody(w, r, keyInfo.Name, requestID)
	if !ok {
		return
	}
	if !json.Valid(body) {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "request body is not valid JSON", http.StatusBadRequest)
		return
	}

	var req imageGenerationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "request body is not a valid image generation request", http.StatusBadRequest)
		return
	}
	requestModel := req.Model
	if strings.TrimSpace(requestModel) == "" || requestModel == "default" || requestModel == "unknown" {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, "a specific model id is required; call GET /v1/models to list available models", http.StatusBadRequest)
		return
	}
	requestModel = g.canonicalCatalogModel(requestModel)
	if !g.checkEndpointKind(w, endpointImages, requestModel) {
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		return
	}
	if g.unknownModel404(w, requestModel) {
		metrics.GatewayRequestsTotal.WithLabelValues("404", "none").Inc()
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	// "url" is served by the gateway (imagehost.go): the worker is always asked for
	// b64_json, the delivered bytes are hosted here and the reply carries links.
	wantURL := false
	switch strings.ToLower(req.ResponseFormat) {
	case "", "b64_json":
	case "url":
		wantURL = true
		body = setJSONField(body, "response_format", "b64_json")
	default:
		metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
		jsonError(w, `response_format must be "b64_json" or "url"`, http.StatusBadRequest)
		return
	}

	// The model must be declared an image model. Its configured UNIT is what makes the
	// billing correct; a token-priced model has no per-image price, and letting one
	// through would price images as tokens (or as zero).
	var unitPrice *big.Float
	if g.settlementCfg != nil {
		if u := g.settlementCfg.UnitFor(requestModel); u.IsToken() {
			metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
			jsonError(w, fmt.Sprintf("model %q is not an image-generation model", requestModel), http.StatusBadRequest)
			return
		}
		if info, found := g.settlementCfg.ModelCatalog[requestModel]; found {
			if cap := info.MaxImagesPerGeneration; cap > 0 && req.N > cap {
				metrics.GatewayRequestsTotal.WithLabelValues("400", "none").Inc()
				jsonError(w, fmt.Sprintf("too many images: %d requested, at most %d", req.N, cap), http.StatusBadRequest)
				return
			}
		}
		unitPrice = g.prices.UnitPrice(requestModel)
		if unitPrice == nil {
			// A non-token model with no per-unit price would bill quantity × nil = $0
			// while the reconciler agreed — an under-charge that never shows as drift.
			// Config validation refuses this at start-up; refuse it here too rather
			// than serve free images if a catalog were ever built another way.
			metrics.GatewayRequestsTotal.WithLabelValues("500", "none").Inc()
			jsonError(w, "model is misconfigured: no per-unit price", http.StatusInternalServerError)
			return
		}
	}

	// Reserve the ESTIMATED cost (requested count × unit price), then adjust to the
	// actual once the real image count is known. Over-reserving for the requested count
	// is safe: the deferred adjustment reverses the difference.
	settledQty := 0
	if g.balanceChecker != nil && keyInfo.Wallet != "" {
		if suspended, debt := g.balanceChecker.IsSuspended(keyInfo.Wallet); suspended {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			g.logger.Warn("request refused: wallet suspended for unpaid debt",
				"api_key", keyInfo.Name, "wallet", keyInfo.Wallet,
				"debt_usd", debt.Text('f', 6), "request_id", requestID)
			jsonError(w, "account suspended: outstanding debt must be settled (top up your balance and wait for the next settlement cycle)", http.StatusPaymentRequired)
			return
		}
		estimatedCostUSD := new(big.Float).Mul(unitPrice, big.NewFloat(float64(req.N)))
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
		if !g.balanceChecker.Reserve(keyInfo.Wallet, estimatedCostUSD) {
			metrics.GatewayRequestsTotal.WithLabelValues("402", "none").Inc()
			jsonError(w, "insufficient balance", http.StatusPaymentRequired)
			return
		}
		defer func() {
			// settledQty is set only on a billable outcome, so any failure path fully
			// reverses the reservation — the same deferred-adjustment discipline that
			// keeps pendingSpend drainable (see adjustBalance).
			g.adjustBalance(keyInfo.Wallet, estimatedCostUSD, requestModel, tokenUsage{}, settledQty)
		}()
	}

	target, err := g.selectWithAffinity("", requestModel)
	if err != nil {
		// No worker serves it right now — wait within the queue window, as the chat
		// path does, so a brief WinningPoSt stay stays transparent.
		target, err = g.blockForWorker(r.Context(), requestModel, time.Now().Add(g.queueWindowFor(requestModel)))
		if err != nil {
			metrics.GatewayRequestsTotal.WithLabelValues("503", "none").Inc()
			// The recovery estimate of the workers that serve this model, as the chat
			// path gives: a fixed 5 s sent clients back every 5 s through a 20-minute
			// WindowPoSt of the only worker holding the model.
			w.Header().Set("Retry-After", g.retryAfterFor(requestModel))
			jsonError(w, "no worker available for this model", http.StatusServiceUnavailable)
			return
		}
	}
	g.noteSwitch(target, requestModel, requestID)

	t0 := time.Now()
	respBody, status, upstreamHeader, workerID, target, sent, err := g.forwardWithRetry(
		r, requestModel, body, "application/json", target, requestID)
	if err != nil {
		g.logger.Error("image generation proxy error", "request_id", requestID,
			"worker", workerID, "error", err)
		metrics.GatewayRequestsTotal.WithLabelValues("502", workerID).Inc()
		jsonError(w, "upstream worker unreachable", http.StatusBadGateway)
		return
	}
	elapsed := time.Since(t0)

	// The billable quantity and the asset digest come from the RESPONSE, never from the
	// request: what was rendered is what gets charged. A worker that returned fewer
	// images than asked is paid for fewer; one that returned more is found out.
	quantity, deliveredHash := countAndHashImages(respBody)

	var receipt *ReceiptInfo
	if status == http.StatusOK && target.HasFeature(worker.FeatureReceipt) {
		receipt = g.captureReceipt(upstreamHeader.Get("X-Om-Receipt"), workerID,
			target.ReceiptPubkey, sent, tokenUsage{AssetQty: quantity}, true, requestID)
		// Cross-check the attested digest against the bytes actually delivered. The
		// signature proves the worker CLAIMED a digest; recomputing it here is what
		// makes the claim checkable instead of merely signed.
		if receipt != nil && receipt.AssetHash != "" && receipt.AssetHash != deliveredHash {
			receipt.Verified = false
			receipt.VerifyError = "attested asset hash differs from the delivered images"
			metrics.ReceiptsTotal.WithLabelValues("invalid").Inc()
			g.logger.Error("image receipt asset hash mismatch (byzantine evidence)",
				"request_id", requestID, "worker", workerID,
				"attested", receipt.AssetHash, "delivered", deliveredHash)
		}
	}

	settledQty = quantity

	statusStr := fmt.Sprintf("%d", status)
	metrics.GatewayRequestsTotal.WithLabelValues(statusStr, workerID).Inc()
	metrics.GatewayRequestDuration.WithLabelValues(workerID).Observe(elapsed.Seconds())
	g.logger.Info("gateway image request",
		"request_id", requestID, "api_key", keyInfo.Name, "worker", workerID,
		"model", requestModel, "status", status,
		"images", quantity, "duration_ms", elapsed.Milliseconds())

	g.logRequest(RequestRecord{
		RequestID: requestID, Timestamp: t0, APIKeyName: keyInfo.Name,
		Wallet: keyInfo.Wallet, WorkerID: workerID, Path: r.URL.Path,
		Model: requestModel, Status: status, DurationMs: elapsed.Milliseconds(),
		Receipt: receipt, Quantity: quantity, AssetHash: deliveredHash,
	})

	copyResponseHeaders(w.Header(), upstreamHeader)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("X-Request-ID", requestID)
	respBody = normalizeErrorBody(status, respBody, w.Header())
	if wantURL && status == http.StatusOK {
		respBody = g.imagesToURLs(r, respBody)
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(status)
	w.Write(respBody)
}

// setJSONField returns body with one top-level field replaced (body unchanged if it is
// not a JSON object).
func setJSONField(body []byte, key string, value any) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	v, err := json.Marshal(value)
	if err != nil {
		return body
	}
	m[key] = v
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// countAndHashImages derives the billable image count and the asset digest from a
// response body, by decoding each returned base64 image and hashing the concatenated
// bytes in order.
//
// A body that is not the expected shape yields (0, ""): the caller then bills nothing,
// which is the safe direction (the client did not receive images either).
func countAndHashImages(respBody []byte) (int, string) {
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, ""
	}
	h := sha256.New()
	count := 0
	for _, item := range parsed.Data {
		if item.B64JSON == "" {
			continue
		}
		raw, err := base64Decode(item.B64JSON)
		if err != nil {
			continue
		}
		h.Write(raw)
		count++
	}
	if count == 0 {
		return 0, ""
	}
	return count, hex.EncodeToString(h.Sum(nil))
}

// base64Decode accepts standard and URL-safe base64, with or without padding: the wire
// form is produced by another language's encoder, and rejecting a valid variant over a
// padding difference would silently drop an image from the bill.
func base64Decode(s string) ([]byte, error) {
	// Unpadded variants are tried by adding the padding back, which is what a Python or
	// Node encoder emitting raw base64 requires.
	candidates := []string{s}
	if n := len(s) % 4; n != 0 {
		candidates = append(candidates, s+strings.Repeat("=", 4-n))
	}
	for _, c := range candidates {
		if raw, err := base64.StdEncoding.DecodeString(c); err == nil {
			return raw, nil
		}
		if raw, err := base64.URLEncoding.DecodeString(c); err == nil {
			return raw, nil
		}
		if raw, err := base64.RawStdEncoding.DecodeString(c); err == nil {
			return raw, nil
		}
		if raw, err := base64.RawURLEncoding.DecodeString(c); err == nil {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

// readBody reads and size-caps the request body, rejecting an oversized one with 413
// rather than truncating it (a truncated body is invalid JSON and would zero the bill).
func (g *Gateway) readBody(w http.ResponseWriter, r *http.Request, keyName, requestID string) ([]byte, bool) {
	bodyCap := g.maxRequestBytes
	if bodyCap <= 0 {
		bodyCap = maxRequestBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyCap+1))
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return nil, false
	}
	if int64(len(body)) > bodyCap {
		metrics.RateLimitRejectedTotal.WithLabelValues("body_too_large").Inc()
		g.logger.Warn("request body exceeds limit", "api_key", keyName,
			"request_id", requestID, "limit_bytes", bodyCap)
		jsonError(w, fmt.Sprintf("request body exceeds limit of %d bytes", bodyCap), http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}
