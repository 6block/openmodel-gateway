package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Both sides of the billing proof must reproduce the shared cross-language fixture.
// Regenerate it with scripts/gen-canonical-vectors.py after any layout change; the diff
// is the review surface for a protocol change.

func TestCanonicalVectors_Leaves(t *testing.T) {
	vecs, err := LeafVectors("testdata")
	if err != nil {
		t.Fatalf("load canonical vectors: %v", err)
	}
	if len(vecs) == 0 {
		t.Fatal("fixture has no leaf vectors")
	}
	for _, lv := range vecs {
		str := func(k string) string { s, _ := lv.Inputs[k].(string); return s }
		num := func(k string) int {
			f, ok := lv.Inputs[k].(float64)
			if !ok {
				return 0
			}
			return int(f)
		}
		var got [32]byte
		switch lv.Shape {
		case "token":
			got = ReceiptLeaf(str("rid"), str("wallet"), str("sp"), str("model"),
				num("prompt_tokens"), num("completion_tokens"), num("cached_tokens"), str("sig"))
		case "asset":
			got = AssetLeaf(str("rid"), str("wallet"), str("sp"), str("model"),
				PriceUnit(str("price_unit")), num("quantity"), str("asset_hash"), str("sig"))
		case "debt":
			got = DebtLeaf(str("rid"), str("wallet"), str("sp"))
		default:
			t.Fatalf("%s: unknown shape %q", lv.Name, lv.Shape)
		}
		if hex.EncodeToString(got[:]) != lv.Leaf {
			t.Errorf("%s (%s): leaf drifted from the cross-language fixture\n want %s\n got  %s",
				lv.Name, lv.Shape, lv.Leaf, hex.EncodeToString(got[:]))
		}
	}
}

// Guard the intent, not just today's bytes: the token and asset shapes must never
// coincide, or a record of one unit could be presented as the other unnoticed.
func TestCanonicalVectors_TokenAndAssetShapesDiffer(t *testing.T) {
	vecs, err := LeafVectors("testdata")
	if err != nil {
		t.Fatalf("load canonical vectors: %v", err)
	}
	byShape := map[string]string{}
	for _, lv := range vecs {
		byShape[lv.Shape] = lv.Leaf
	}
	if byShape["token"] == byShape["asset"] {
		t.Fatal("fixture has a token leaf equal to an asset leaf — the unit must be part of the commitment")
	}
}

var _ = sha256.Sum256

// TestCharacterCountMatchesFixture: the TTS billing quantity must equal Python's
// len(str) on every edge case in the shared fixture — the worker signs its count and
// the gateway checks it against this one.
func TestCharacterCountMatchesFixture(t *testing.T) {
	v, err := LoadCanonicalVectors("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.CharacterCounts) == 0 {
		t.Fatal("fixture has no character_counts")
	}
	for _, c := range v.CharacterCounts {
		if got := CharacterCount(c.Text); got != c.Count {
			t.Errorf("CharacterCount(%q) = %d, Python len() = %d", c.Text, got, c.Count)
		}
	}
}
