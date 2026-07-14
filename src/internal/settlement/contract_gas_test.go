package settlement

import (
	"math/big"
	"testing"
)

func TestBumpGasPrice(t *testing.T) {
	if got := bumpGasPrice(big.NewInt(100), 25); got.Cmp(big.NewInt(125)) != 0 {
		t.Errorf("bump 100 by 25%% = %s, want 125", got)
	}
	if got := bumpGasPrice(big.NewInt(100), 13); got.Cmp(big.NewInt(113)) != 0 {
		t.Errorf("bump 100 by 13%% = %s, want 113", got)
	}
	if got := bumpGasPrice(nil, 25); got != nil {
		t.Errorf("bump nil = %v, want nil", got)
	}
	if got := bumpGasPrice(big.NewInt(0), 25); got.Sign() != 0 {
		t.Errorf("bump 0 = %s, want 0", got)
	}
}

// TestDecideNonceGas covers the nonce/gas selection that fixes the audit HIGH
// "no nonce/gas management → stuck tx deadlock".
func TestDecideNonceGas(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }

	// 1. No prior unconfirmed tx: use the network's pending nonce + gas headroom.
	nonce, gas := decideNonceGas(5, big.NewInt(100), nil, nil)
	if nonce != 5 {
		t.Errorf("fresh nonce = %d, want 5", nonce)
	}
	if gas.Cmp(big.NewInt(125)) != 0 {
		t.Errorf("fresh gas = %s, want 125 (suggested+25%%)", gas)
	}

	// 2. Prior tx still pending (network nonce hasn't advanced past it): REPLACE it
	//    — same nonce, gas = max(suggested+25%, prior+13%). Here suggested rose, so
	//    headroom wins.
	nonce, gas = decideNonceGas(7, big.NewInt(200), u(7), big.NewInt(100))
	if nonce != 7 {
		t.Errorf("RBF nonce = %d, want 7 (replace stuck tx)", nonce)
	}
	if gas.Cmp(big.NewInt(250)) != 0 { // 200+25% = 250 > 100+13% = 113
		t.Errorf("RBF gas = %s, want 250", gas)
	}

	// 3. Prior tx still pending but suggested gas is flat/low: the replacement
	//    minimum (prior+13%) must win, else the node rejects "underpriced".
	nonce, gas = decideNonceGas(7, big.NewInt(80), u(7), big.NewInt(100))
	if nonce != 7 {
		t.Errorf("RBF nonce = %d, want 7", nonce)
	}
	if gas.Cmp(big.NewInt(113)) != 0 { // max(80+25%=100, 100+13%=113) = 113
		t.Errorf("RBF gas = %s, want 113 (prior+13%% replacement floor)", gas)
	}

	// 4. Prior tx already mined (pending advanced past it): use the fresh nonce.
	nonce, gas = decideNonceGas(8, big.NewInt(100), u(7), big.NewInt(100))
	if nonce != 8 {
		t.Errorf("post-mine nonce = %d, want 8 (fresh)", nonce)
	}
	if gas.Cmp(big.NewInt(125)) != 0 {
		t.Errorf("post-mine gas = %s, want 125", gas)
	}
}
