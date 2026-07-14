package admin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// F4: withScanLimit must bound how many heavy log-scan handlers run at once, and a
// cancelled request (client gone / timeout) must abort the wait instead of piling up.
func TestWithScanLimit_BoundsConcurrency(t *testing.T) {
	sa := &SettlementAPI{scanSem: make(chan struct{}, 2)}

	var inFlight, maxSeen int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sa.withScanLimit(context.Background(), func() {
				n := atomic.AddInt32(&inFlight, 1)
				for {
					m := atomic.LoadInt32(&maxSeen)
					if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&inFlight, -1)
			})
		}()
	}
	wg.Wait()
	if maxSeen > 2 {
		t.Fatalf("scan concurrency must be bounded to 2, peaked at %d", maxSeen)
	}
}

func TestWithScanLimit_CancelAbortsWait(t *testing.T) {
	sa := &SettlementAPI{scanSem: make(chan struct{}, 1)}
	// Occupy the only slot.
	sa.scanSem <- struct{}{}
	defer func() { <-sa.scanSem }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled request
	ran := false
	err := sa.withScanLimit(ctx, func() { ran = true })
	if err == nil {
		t.Fatal("a cancelled request must not wait indefinitely for a scan slot")
	}
	if ran {
		t.Fatal("fn must not run when the wait was aborted")
	}
}

// nil semaphore (defensive) runs inline.
func TestWithScanLimit_NilRunsInline(t *testing.T) {
	sa := &SettlementAPI{}
	ran := false
	if err := sa.withScanLimit(context.Background(), func() { ran = true }); err != nil || !ran {
		t.Fatalf("nil sem must run inline: err=%v ran=%v", err, ran)
	}
}
