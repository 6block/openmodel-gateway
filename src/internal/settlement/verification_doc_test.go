package settlement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The byte-level specification in docs/verification.md is what an external verifier
// implements from. Nothing at runtime reads it, so a change to a canonical template that
// does not update the document leaves the published spec silently wrong — the worst
// possible failure for a scheme whose whole point is third-party re-verification.
//
// These tests read the templates out of the document and assert the exact byte strings
// this package actually hashes. They are deliberately literal: a reformat of the JSON
// (however cosmetic) must be accompanied by a docs edit, because the bytes ARE the
// contract.

// verificationDocPath locates docs/verification.md by walking up from this package.
// Walking up rather than counting ".." keeps the test working from both the working tree
// and the staging repo, which sit at different depths.
func verificationDocPath() string {
	here, err := os.Getwd()
	if err != nil {
		return ""
	}
	rel := filepath.Join("docs", "verification.md")
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(here, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		// The contracts repo carries the same document under its own name.
		candidate = filepath.Join(here, "repo-openmodel-contracts", rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(here)
		if parent == here {
			break
		}
		here = parent
	}
	return ""
}

func readVerificationDoc(t *testing.T) string {
	t.Helper()
	p := verificationDocPath()
	if p == "" {
		t.Skip("docs/verification.md not reachable from this package")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestVerificationDocHasTokenTemplates: the document must state the token payload and
// token leaf templates exactly as the code builds them.
func TestVerificationDocHasTokenTemplates(t *testing.T) {
	doc := readVerificationDoc(t)
	want := []string{
		// Token signing payload (receipt.go / receipt.py).
		`"prompt_tokens":<int>,"pubkey":<json-str>,"request_id":<json-str>`,
		`"request_sha256":<json-str>,"response_sha256":<json-str>,"ts":<int>,"v":1}`,
		// Token leaf (ReceiptLeaf).
		`{"cached_tokens":<int>,"completion_tokens":<int>,"model":js,`,
		`"prompt_tokens":<int>,"request_id":js,"sig":js,"sp":js,"wallet":js}`,
		// Debt leaf (DebtLeaf).
		`{"debt":true,"request_id":js,"sp":js,"wallet":js}`,
	}
	for _, w := range want {
		if !strings.Contains(doc, w) {
			t.Errorf("verification.md no longer documents this template literally:\n  %s", w)
		}
	}
}

// TestVerificationDocHasAssetTemplates: a non-token unit signs and commits a different
// shape. An external verifier that is not told about it cannot verify an image receipt
// at all — so the document must carry both the asset payload and the asset leaf.
func TestVerificationDocHasAssetTemplates(t *testing.T) {
	doc := readVerificationDoc(t)
	want := []string{
		// Asset signing payload (receipt.go / receipt.py, quantity > 0).
		`{"asset_hash":<json-str>,"model":<json-str>,"quantity":<int>,`,
		`"response_sha256":<json-str>,"ts":<int>,"v":1}`,
		// Asset leaf (AssetLeaf).
		`{"asset_hash":js,"model":js,"price_unit":js,"quantity":<int>,`,
		`"request_id":js,"sig":js,"sp":js,"wallet":js}`,
	}
	for _, w := range want {
		if !strings.Contains(doc, w) {
			t.Errorf("verification.md no longer documents this template literally:\n  %s", w)
		}
	}
}

// TestVerificationDocStatesAdminFieldOrdering: the templates are order-sensitive (the
// bytes are hashed, not parsed and re-serialised), and both are in ASCII key order. A
// document that listed the fields in a different order would lead a verifier to build
// bytes that never verify.
func TestVerificationDocStatesAdminFieldOrdering(t *testing.T) {
	doc := readVerificationDoc(t)
	if !strings.Contains(doc, "fixed-template canonical JSON") &&
		!strings.Contains(doc, "fixed template") {
		t.Error("verification.md must state that the templates are fixed, so field order is part of the format")
	}
	if !strings.Contains(doc, "ASCII") {
		t.Error("verification.md should note the keys are in ASCII order")
	}
}

// TestVerificationDocWarnsAboutUTF8Encoding: Go emits raw UTF-8 while Python escapes by
// default. That divergence breaks every non-ASCII model name, so the spec has to call it
// out for anyone reimplementing a verifier.
func TestVerificationDocWarnsAboutUTF8Encoding(t *testing.T) {
	doc := readVerificationDoc(t)
	if !strings.Contains(doc, "ensure_ascii=False") && !strings.Contains(doc, "raw UTF-8") {
		t.Error("verification.md must specify the string encoding (raw UTF-8, not \\uXXXX escapes)")
	}
}

// TestVerificationDocNamesEveryUnit: an external verifier needs to know what quantity
// and asset_hash mean for each billing unit. A unit the code bills but the spec does
// not describe is an unverifiable charge.
func TestVerificationDocNamesEveryUnit(t *testing.T) {
	doc := readVerificationDoc(t)
	for _, u := range []PriceUnit{PriceUnitImage, PriceUnitAudioSecond, PriceUnitCharacter} {
		if !strings.Contains(doc, "`"+string(u)+"`") {
			t.Errorf("verification.md does not describe billing unit %q", u)
		}
	}
	if !strings.Contains(doc, "code points") {
		t.Error("verification.md must define the character count as Unicode code points")
	}
}
