package settlement

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func receiptAtBlock(block uint64) *types.Receipt {
	return &types.Receipt{BlockNumber: new(big.Int).SetUint64(block)}
}

// A transient NotFound (Filecoin head-tipset wobble) must NOT be misread as a reorg:
// once the receipt reappears and is buried deep enough, finality succeeds. This is the
// false-positive the 24h soak surfaced (was firing "reorged" on ~90% of settlements).
func TestWaitForFinality_TransientNotFoundIsNotReorg(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, h common.Hash) (*types.Receipt, uint64, error) {
		calls++
		if calls <= 2 {
			return nil, 0, ethereum.NotFound // transient miss on the first couple of polls
		}
		return receiptAtBlock(100), 105, nil // reappears, buried >= 5 deep
	}
	r, err := waitForFinality(context.Background(), common.Hash{}, 5, 2*time.Second, time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("transient NotFound must not be treated as a reorg, got err=%v", err)
	}
	if r == nil || r.BlockNumber.Uint64() != 100 {
		t.Fatalf("expected final receipt at block 100, got %v", r)
	}
}

// A receipt that stays absent across the whole streak IS a real reorg → ErrReorged.
func TestWaitForFinality_PersistentNotFoundIsReorg(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, h common.Hash) (*types.Receipt, uint64, error) {
		calls++
		return nil, 0, ethereum.NotFound // never comes back
	}
	_, err := waitForFinality(context.Background(), common.Hash{}, 5, 2*time.Second, time.Millisecond, fetch)
	if !errors.Is(err, ErrReorged) {
		t.Fatalf("persistent NotFound must return ErrReorged, got %v", err)
	}
	if calls < maxNotFoundStreak {
		t.Errorf("expected at least %d polls before declaring reorg, got %d", maxNotFoundStreak, calls)
	}
}

// A present-but-not-yet-buried receipt keeps polling until confirmation depth is reached.
func TestWaitForFinality_WaitsForDepth(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, h common.Hash) (*types.Receipt, uint64, error) {
		calls++
		return receiptAtBlock(100), uint64(100 + calls), nil // head advances 1 block per poll
	}
	r, err := waitForFinality(context.Background(), common.Hash{}, 5, 2*time.Second, time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r == nil {
		t.Fatal("expected a final receipt once buried deep enough")
	}
	if calls < 5 {
		t.Errorf("should have polled until depth 5 was reached, only polled %d times", calls)
	}
}
