package settlement

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// receipt_status.go — the three-state answer behind the public receipt-proof
// endpoint.
//
// "Not settled yet" and "no such request" are different answers to a user, but
// the endpoint used to collapse both into one 404 error string. A user who
// opens the receipt link right after a chat reply — the single most likely
// moment to click it — always landed on that error, reading as if their money
// or their receipt was lost, when the batch simply had not been committed yet
// (settlement runs on a fixed interval). This file lets the endpoint say
// "recorded, signed, awaiting the next on-chain batch" with an ETA instead.

// UnsettledStatus is what we know about a request that is billed but whose
// settlement batch has not yet been committed on-chain.
type UnsettledStatus struct {
	Record RequestRecord // the billing-log row, including the worker receipt if any
}

// noteSettleRan stamps the completion time of a settle pass; SettleETA uses it
// to answer "how long until the next pass".
func (s *Settler) noteSettleRan() { atomic.StoreInt64(&s.lastSettleUnix, time.Now().Unix()) }

// SettleETA returns the estimated seconds until the next settlement pass.
// Before the first pass completes it can only promise the full interval.
func (s *Settler) SettleETA() int64 {
	interval := int64(s.cfg.IntervalMinutes) * 60
	if interval <= 0 {
		return 0
	}
	last := atomic.LoadInt64(&s.lastSettleUnix)
	if last == 0 {
		return interval
	}
	eta := last + interval - time.Now().Unix()
	if eta < 0 {
		return 0
	}
	return eta
}

// FindUnsettled scans the request log (current file plus its most recent
// rotation) for a billed-but-not-yet-committed request. It is only consulted
// AFTER the Merkle index missed, i.e. for requests younger than one settlement
// interval, which live at the tail of the current log; the .1 rotation covers a
// click that races a 50MB rollover. Callers wrap this in withScanLimit.
func (s *Settler) FindUnsettled(rid string) (*UnsettledStatus, bool) {
	if rid == "" {
		return nil, false
	}
	needle := `"` + rid + `"`
	for _, path := range []string{s.requestLogPath, s.requestLogPath + ".1"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.Contains(line, needle) {
				continue
			}
			var rec RequestRecord
			if json.Unmarshal([]byte(line), &rec) != nil || rec.RequestID != rid {
				continue
			}
			f.Close()
			return &UnsettledStatus{Record: rec}, true
		}
		f.Close()
	}
	return nil, false
}
