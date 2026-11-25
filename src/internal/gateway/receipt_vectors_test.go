package gateway

import (
	"testing"

	"openmodel/sp-state-agent/internal/settlement"
)

// Assert the gateway's own canonicalReceiptPayload reproduces the payloads recorded in
// the shared cross-language fixture (generated from the PYTHON signer, see
// scripts/gen-canonical-vectors.py).
//
// This is the Go half of the two-language contract. It is the test that would have
// caught a real defect: Python's json.dumps escapes non-ASCII by default while Go's
// json.Marshal emits raw UTF-8, so a model with a non-ASCII name produced a payload
// Python signed and Go rebuilt differently — every receipt for that model would have
// failed verification, with all tests green.
func TestCanonicalVectors_ReceiptPayloads(t *testing.T) {
	vecs, err := settlement.PayloadVectors("../settlement/testdata")
	if err != nil {
		t.Fatalf("load canonical vectors: %v", err)
	}
	for _, v := range vecs {
		in := v.Inputs
		str := func(k string) string { s, _ := in[k].(string); return s }
		num := func(k string) int {
			f, ok := in[k].(float64) // JSON numbers decode as float64
			if !ok {
				return 0
			}
			return int(f)
		}
		r := &ReceiptInfo{
			V:                1,
			RequestID:        str("request_id"),
			Model:            str("model"),
			RequestSHA256:    str("request_sha256"),
			ResponseSHA256:   str("response_sha256"),
			PromptTokens:     num("prompt_tokens"),
			CompletionTokens: num("completion_tokens"),
			CachedTokens:     num("cached_tokens"),
			Quantity:         num("quantity"),
			AssetHash:        str("asset_hash"),
			TS:               int64(num("ts")),
			Pubkey:           str("pubkey"),
		}
		got := string(canonicalReceiptPayload(r))
		if got != v.Payload {
			t.Errorf("%s (%s): payload drifted from the cross-language fixture — "+
				"receipts signed by the Python worker would stop verifying\n want %s\n got  %s",
				v.Name, v.Shape, v.Payload, got)
		}
	}
}
