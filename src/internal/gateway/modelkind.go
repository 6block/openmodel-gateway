package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openmodel/sp-state-agent/internal/settlement"
	"openmodel/sp-state-agent/internal/worker"
)

// modelkind.go — one answer, for every endpoint, to two questions about a requested
// model: which catalog entry is it, and may it be used HERE.
//
// Catalog key. A client may name a model by its HuggingFace id
// ("hexgrad/Kokoro-82M", the spelling /v1/models lists) or by its local weight path
// ("/models/hexgrad--Kokoro-82M", the spelling the catalog is keyed by). Routing has
// always treated the two as one model; pricing looked the raw string up and, missing,
// fell back to the "default" token price and token unit. So a speech or image model
// named by its listed id was refused as "not a speech model", and a text model priced
// in the catalog was billed at the default rate. canonicalCatalogModel folds the
// request onto the catalog's own key before any price, unit, cap or log record uses it.
//
// Endpoint kind. Each endpoint serves one family of model. A request for a model of
// another family is the client's mistake and gets a 400 naming the right endpoint —
// before reservation, routing or forwarding. Without this check a chat request for an
// image model reached the image worker, returned an empty 200, and a chat request for a
// speech model surfaced as a 500 or 502 from the worker.

// endpointKind is the family of models an endpoint serves.
type endpointKind int

const (
	endpointChat endpointKind = iota // chat/completions and completions: text + vision
	endpointImages
	endpointTranscription
	endpointSpeech
)

// endpointFor names, per modality, the endpoint that serves it — used in the refusal
// message so a client is told where to go, not only that it is wrong here.
var endpointFor = map[string]string{
	settlement.ModalityText:   "/v1/chat/completions",
	settlement.ModalityVision: "/v1/chat/completions",
	settlement.ModalityImage:  "/v1/images/generations",
	settlement.ModalityASR:    "/v1/audio/transcriptions",
	settlement.ModalityTTS:    "/v1/audio/speech",
}

func (k endpointKind) accepts(modality string) bool {
	switch k {
	case endpointChat:
		return modality == settlement.ModalityText || modality == settlement.ModalityVision
	case endpointImages:
		return modality == settlement.ModalityImage
	case endpointTranscription:
		return modality == settlement.ModalityASR
	case endpointSpeech:
		return modality == settlement.ModalityTTS
	}
	return false
}

// canonicalCatalogModel returns the catalog key the requested model refers to, or the
// request unchanged when the catalog has no entry for it under any spelling.
func (g *Gateway) canonicalCatalogModel(model string) string {
	if g.settlementCfg == nil || model == "" {
		return model
	}
	if _, ok := g.settlementCfg.ModelCatalog[model]; ok {
		return model
	}
	if _, ok := g.settlementCfg.ModelPricesUSD[model]; ok {
		return model
	}
	want := normalizeModelKey(canonicalModelID(model))
	for key := range g.settlementCfg.ModelCatalog {
		if key != "default" && normalizeModelKey(canonicalModelID(key)) == want {
			return key
		}
	}
	for key := range g.settlementCfg.ModelPricesUSD {
		if key != "default" && normalizeModelKey(canonicalModelID(key)) == want {
			return key
		}
	}
	return model
}

// modalityOf is the configured modality of a (canonical) model; text for a model with
// no catalog entry, as everywhere else.
func (g *Gateway) modalityOf(model string) string {
	if g.settlementCfg == nil {
		return settlement.ModalityText
	}
	if info, ok := g.settlementCfg.ModelCatalog[model]; ok {
		return info.EffectiveModality()
	}
	return settlement.ModalityText
}

// checkEndpointKind refuses a model of the wrong family with a 400 that names the
// endpoint which serves it. It reports whether the request may proceed.
//
// A model the gateway knows nothing about (no catalog entry, no worker claiming it)
// passes: the endpoint's own later stages answer it — on chat, after the balance gates,
// with "no worker supports model"; on the per-unit endpoints with the same 404 from
// unknownModel404 — so the order of 402 before 404 is the same everywhere.
func (g *Gateway) checkEndpointKind(w http.ResponseWriter, kind endpointKind, model string) bool {
	if !g.knownModel(model) {
		return true
	}
	mod := g.modalityOf(model)
	if kind.accepts(mod) {
		return true
	}
	jsonError(w, fmt.Sprintf("model %q is %s model; use %s", model, modalityNoun[mod], endpointFor[mod]), http.StatusBadRequest)
	return false
}

var modalityNoun = map[string]string{
	settlement.ModalityText:   "a text",
	settlement.ModalityVision: "an image-understanding",
	settlement.ModalityImage:  "an image-generation",
	settlement.ModalityASR:    "a speech-recognition",
	settlement.ModalityTTS:    "a speech-synthesis",
}

// knownModel: configured in the catalog or price table, or claimed by some worker.
func (g *Gateway) knownModel(model string) bool {
	if g.settlementCfg != nil {
		if _, ok := g.settlementCfg.ModelCatalog[model]; ok {
			return true
		}
		if _, ok := g.settlementCfg.ModelPricesUSD[model]; ok {
			return true
		}
	}
	return g.registry == nil || anyWorkerSupportsModel(g.registry, model)
}

// normalizeErrorBody gives every non-2xx reply the one error shape clients parse:
// {"error":{"message":..., "type":...}}. The gateway's own errors already have it; a
// worker's do not always — FastAPI answers {"detail": "..."} — so a client calling
// two endpoints would otherwise need two error parsers. A body already in the shape is
// returned unchanged; anything else has its text kept as the message.
func normalizeErrorBody(status int, body []byte, header http.Header) []byte {
	if status < 400 {
		return body
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) == nil {
		if e, ok := probe["error"]; ok {
			var inner struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(e, &inner) == nil && inner.Message != "" {
				return body
			}
		}
	}
	msg := strings.TrimSpace(string(body))
	if d, ok := probe["detail"]; ok {
		var s string
		if json.Unmarshal(d, &s) == nil {
			msg = s
		} else {
			msg = string(d)
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	typ := "upstream_error"
	if status == http.StatusServiceUnavailable {
		typ = "service_unavailable"
	}
	out, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
	header.Set("Content-Type", "application/json")
	header.Del("Content-Length")
	return append(out, '\n')
}

// unknownModel404 answers a per-unit endpoint's request for a model nothing serves,
// with the same 404 chat gives. Reports whether it answered.
func (g *Gateway) unknownModel404(w http.ResponseWriter, model string) bool {
	if g.knownModel(model) {
		return false
	}
	jsonError(w, fmt.Sprintf("no worker supports model %q", model), http.StatusNotFound)
	return true
}

// forwardWithRetry sends a per-unit (image / audio) request to target and, when that
// worker could not serve it — transport error, 503 (yielded to mining mid-render) or
// 502/504 (its front could not reach the engine) — retries on another worker that
// serves the model, waiting in the queue window when none is free. Nothing was
// produced by a failed attempt, so nothing is billed for it; the body is re-sent
// byte-for-byte (the receipt hashes it). The chat path has had this loop since M2;
// these endpoints passed the first worker's failure straight to the client.
// sent is the exact body the serving worker received — what its receipt hashes.
func (g *Gateway) forwardWithRetry(r *http.Request, model string, body []byte, contentType string,
	target *worker.Worker, requestID string) (respBody []byte, status int, header http.Header, workerID string, served *worker.Worker, sent []byte, err error) {
	tried := map[string]bool{}
	deadline := time.Now().Add(g.requestTimeout)
	for attempt := 0; ; attempt++ {
		send := body
		if m, ok := loadedMatch(target, model); ok && contentType == "application/json" {
			send = rewriteModelField(body, m)
		}
		func() {
			g.registry.IncInflight(target.ID)
			defer g.registry.DecInflight(target.ID)
			respBody, status, header, workerID, err = g.forwardRequestAs(r.Context(), r.URL.Path, send, contentType, target, requestID)
		}()
		retryable := err != nil || status == http.StatusServiceUnavailable ||
			status == http.StatusBadGateway || status == http.StatusGatewayTimeout
		if !retryable || r.Context().Err() != nil || errors.Is(err, errResponseTooLarge) {
			return respBody, status, header, workerID, target, send, err
		}
		g.logger.Warn("worker could not serve the request, retrying on another",
			"request_id", requestID, "worker", workerID, "status", status, "error", err, "attempt", attempt+1)
		if err != nil || status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
			g.registry.MarkUnreachable(target.ID)
		} else {
			g.registry.MarkYielding(target.ID)
		}
		tried[target.ID] = true
		next, serr := selectWorkerForModel(g.registry, model, tried)
		if serr != nil {
			if time.Now().After(deadline) {
				return respBody, status, header, workerID, target, send, err
			}
			// Every servable worker was tried: wait for one to come back, then start over.
			wait := deadline
			if q := time.Now().Add(g.queueTimeout); q.Before(wait) {
				wait = q
			}
			next, serr = g.blockForWorker(r.Context(), model, wait)
			if serr != nil {
				return respBody, status, header, workerID, target, send, err
			}
			tried = map[string]bool{}
		}
		target = next
		g.noteSwitch(target, model, requestID)
	}
}
