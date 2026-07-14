package gateway

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"openmodel/sp-state-agent/internal/metrics"
)

// receipt.go — gateway side of A1 signed inference receipts.
//
// The worker attests each served request (request hash, response hash, token counts)
// with an ed25519 signature; the gateway verifies it against the pubkey the worker
// advertises on /health and stores it in the billing ledger. Settlement then commits a
// Merkle root over receipts into the on-chain batch hash, making every charge
// user-verifiable end-to-end. A failed verification never fails the request (the client
// is already served) — it is recorded as byzantine evidence (flag + metric + log).

// ReceiptInfo is a worker-signed receipt as stored in the billing ledger.
type ReceiptInfo struct {
	V                int    `json:"v"`
	RequestID        string `json:"request_id"`
	Model            string `json:"model"`
	RequestSHA256    string `json:"request_sha256"`
	ResponseSHA256   string `json:"response_sha256"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	TS               int64  `json:"ts"`
	Pubkey           string `json:"pubkey"`
	Sig              string `json:"sig"`
	// Verified is the gateway's verdict at capture time; VerifyError says why not.
	Verified    bool   `json:"verified"`
	VerifyError string `json:"verify_error,omitempty"`
}

// canonicalReceiptPayload rebuilds the EXACT bytes the worker signed. Fixed field
// order, string values individually JSON-encoded — byte-identical with py-inference
// src/inference/receipt.py canonical_payload. Any drift breaks every receipt.
func canonicalReceiptPayload(r *ReceiptInfo) []byte {
	js := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []byte(fmt.Sprintf(
		`{"cached_tokens":%d,"completion_tokens":%d,"model":%s,"prompt_tokens":%d,"pubkey":%s,"request_id":%s,"request_sha256":%s,"response_sha256":%s,"ts":%d,"v":1}`,
		r.CachedTokens, r.CompletionTokens, js(r.Model), r.PromptTokens, js(r.Pubkey),
		js(r.RequestID), js(r.RequestSHA256), js(r.ResponseSHA256), r.TS))
}

// parseReceiptHeader decodes the base64 X-OM-Receipt header value.
func parseReceiptHeader(v string) (*ReceiptInfo, error) {
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	var r ReceiptInfo
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &r, nil
}

// verifyReceipt checks the receipt against everything the gateway knows first-hand:
//   - signature over the canonical payload, using the /health-advertised pubkey
//     (NOT the receipt's embedded pubkey — a byzantine worker could self-sign);
//   - request hash == sha256 of the body the gateway actually sent upstream;
//   - reported tokens == the usage the gateway parsed from the response (when known).
//
// Sets Verified / VerifyError on the receipt and bumps the metric.
func verifyReceipt(r *ReceiptInfo, expectPubkeyHex string, sentBody []byte, u tokenUsage, checkUsage bool) {
	fail := func(why string) {
		r.Verified = false
		r.VerifyError = why
		metrics.ReceiptsTotal.WithLabelValues("invalid").Inc()
	}
	if expectPubkeyHex == "" {
		fail("worker advertises no receipt pubkey")
		return
	}
	if r.Pubkey != expectPubkeyHex {
		fail("receipt pubkey differs from /health-advertised pubkey")
		return
	}
	pub, err := hex.DecodeString(expectPubkeyHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		fail("bad advertised pubkey encoding")
		return
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		fail("bad signature encoding")
		return
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), canonicalReceiptPayload(r), sig) {
		fail("signature does not verify")
		return
	}
	if sentBody != nil {
		want := fmt.Sprintf("%x", sha256.Sum256(sentBody))
		if r.RequestSHA256 != want {
			fail("request hash mismatch (worker attested different request bytes)")
			return
		}
	}
	if checkUsage &&
		(r.PromptTokens != u.PromptTokens || r.CompletionTokens != u.CompletionTokens ||
			r.CachedTokens != u.CachedTokens) {
		fail("receipt token counts differ from response usage")
		return
	}
	r.Verified = true
	metrics.ReceiptsTotal.WithLabelValues("verified").Inc()
}

// captureReceipt parses + verifies a receipt and returns it ready for the ledger.
// A nil return means no receipt was presented (older worker / feature off).
func (g *Gateway) captureReceipt(headerVal, workerID, expectPubkey string,
	sentBody []byte, u tokenUsage, checkUsage bool, requestID string) *ReceiptInfo {
	if headerVal == "" {
		metrics.ReceiptsTotal.WithLabelValues("missing").Inc()
		return nil
	}
	r, err := parseReceiptHeader(headerVal)
	if err != nil {
		g.logger.Error("unparseable inference receipt", "request_id", requestID,
			"worker", workerID, "error", err)
		metrics.ReceiptsTotal.WithLabelValues("invalid").Inc()
		return &ReceiptInfo{RequestID: requestID, Verified: false, VerifyError: "unparseable: " + err.Error()}
	}
	verifyReceipt(r, expectPubkey, sentBody, u, checkUsage)
	if !r.Verified {
		g.logger.Error("inference receipt FAILED verification (byzantine evidence)",
			"request_id", requestID, "worker", workerID, "reason", r.VerifyError)
	}
	return r
}
