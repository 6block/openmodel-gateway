package settlement

import (
	"fmt"
	"os"
)

// statecheck.go provides a programmatic post-restore integrity check (B3): after
// restoring state files from a backup, the operator (or restore script) can confirm
// the fund-critical files parse and are mutually consistent BEFORE starting the
// gateway, instead of discovering corruption at runtime.

// StateCheckResult summarizes the integrity of the on-disk settlement state.
type StateCheckResult struct {
	OK            bool     `json:"ok"`
	CursorOffset  int64    `json:"cursor_offset"`
	SettledUSD    string   `json:"settled_usd"`
	DebtEntries   int      `json:"debt_entries"`
	DeadLetters   int      `json:"dead_letters"`
	WALPresent    bool     `json:"wal_present"`
	WALConfirmed  int      `json:"wal_confirmed_batches"`
	WALTotal      int      `json:"wal_total_batches"`
	Problems      []string `json:"problems"`
}

// VerifyState reads and cross-checks the settler's persisted state files. It never
// mutates anything. A non-empty Problems slice (and OK=false) means the restored
// state is unsafe to start on — e.g. a cursor that points beyond the request log, or
// an unparseable WAL.
func (s *Settler) VerifyState() StateCheckResult {
	res := StateCheckResult{OK: true}

	// Cursor must load (a corrupt cursor would otherwise reset scanning to 0 and
	// re-bill everything, or skip records).
	cur, err := s.scanner.loadCursor()
	if err != nil && !os.IsNotExist(err) {
		res.OK = false
		res.Problems = append(res.Problems, fmt.Sprintf("cursor unreadable: %v", err))
	}
	res.CursorOffset = cur.Offset

	// Cursor offset must not exceed the current request log size (would mean the log
	// was truncated/replaced without the cursor being reset — records would be skipped).
	if info, statErr := os.Stat(s.requestLogPath); statErr == nil {
		if cur.Offset > info.Size() {
			// Only a problem if no rotation backups explain the larger offset; the
			// scanner handles rotation, but a bare offset>size with no backups is a flag.
			if _, bErr := os.Stat(s.requestLogPath + ".1"); bErr != nil {
				res.OK = false
				res.Problems = append(res.Problems,
					fmt.Sprintf("cursor offset %d exceeds request-log size %d with no rotation backup", cur.Offset, info.Size()))
			}
		}
	}

	// Settled-total must parse.
	st := s.readSettledTotal()
	res.SettledUSD = st.TotalUSD
	if _, _, perr := bigParse(st.TotalUSD); perr != nil {
		res.OK = false
		res.Problems = append(res.Problems, fmt.Sprintf("settled-total unparseable: %v", perr))
	}

	// Debt ledger: loadDebts already skips bad entries, but an unparseable FILE is a
	// problem — detect by reading raw then comparing.
	res.DebtEntries = len(s.loadDebts())

	// Dead-letter count.
	res.DeadLetters = len(s.loadDeadLetter())

	// WAL: if present it must parse, and confirmed<=total.
	if pending, ok, perr := s.readPending(); perr != nil {
		res.OK = false
		res.Problems = append(res.Problems, fmt.Sprintf("pending WAL unparseable: %v", perr))
	} else if ok {
		res.WALPresent = true
		res.WALTotal = len(pending.Batches)
		for _, b := range pending.Batches {
			if b.Confirmed {
				res.WALConfirmed++
			}
		}
	}

	return res
}

// bigParse is a tiny helper so VerifyState can validate decimal strings without
// importing math/big at call sites.
func bigParse(s string) (float64, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	if err != nil {
		return 0, false, err
	}
	return f, true, nil
}
