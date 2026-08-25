package settlement

import (
	"math/big"
	"testing"
	"time"
)

func TestIsImplausiblePrice(t *testing.T) {
	// With a last-good price of $2.00 (reference present).
	p := &Pricer{filPriceUSD: big.NewFloat(2.0), lastUpdateTime: time.Now()}
	cases := []struct {
		name  string
		price float64
		want  bool
	}{
		{"small move within band", 2.4, false},    // +20%
		{"big move down within band", 1.1, false}, // -45%
		{"gross spike over band", 5.0, true},      // +150%
		{"flash crash to ~zero", 0.001, true},     // below absolute floor
		{"absurd ceiling", 50000, true},           // above absolute cap
		{"normal", 2.0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.isImplausiblePrice(big.NewFloat(c.price)); got != c.want {
				t.Fatalf("isImplausiblePrice(%v) = %v, want %v", c.price, got, c.want)
			}
		})
	}

	// Bootstrap (no prior price): only the absolute band applies, no deviation check.
	boot := &Pricer{filPriceUSD: big.NewFloat(0)}
	if boot.isImplausiblePrice(big.NewFloat(3.0)) {
		t.Fatal("bootstrap: a plausible absolute price must be accepted")
	}
	if !boot.isImplausiblePrice(big.NewFloat(0)) {
		t.Fatal("bootstrap: zero price must be rejected by absolute band")
	}
}
