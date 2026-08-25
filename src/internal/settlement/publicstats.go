package settlement

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ActiveWalletWindow is the trailing window over which a wallet counts as an
// active developer. Stated explicitly because the number is published as a grant
// metric: the figure decays if a developer stops using the platform rather than
// growing monotonically.
const ActiveWalletWindow = 90 * 24 * time.Hour

// ActiveWalletMinRequests is how many successful billable requests a wallet must
// have inside the window before it counts. A single trial call is someone trying
// the service, not a developer building on it; requiring a floor keeps the metric
// from being inflated by one-off pokes (including our own smoke tests).
const ActiveWalletMinRequests = 10

// activeWalletTTL bounds how often the request log is walked for this count. The
// walk is O(log size) and the metric changes slowly, so serving a slightly stale
// number is preferable to letting a public, unauthenticated endpoint schedule
// disk work on every call.
const activeWalletTTL = 10 * time.Minute

// ActiveWalletCounter counts distinct wallets with at least
// ActiveWalletMinRequests successful billable requests in the trailing window,
// reading the same request logs settlement bills from. Results are cached;
// concurrent callers share one walk.
type ActiveWalletCounter struct {
	logPath string
	now     func() time.Time // injectable for tests

	mu       sync.Mutex
	count    int
	computed time.Time
	inFlight *sync.WaitGroup
}

func NewActiveWalletCounter(requestLogPath string) *ActiveWalletCounter {
	return &ActiveWalletCounter{logPath: requestLogPath, now: time.Now}
}

// Count returns the cached count and the time it was computed. A zero `asOf`
// means no successful walk has happened yet (the caller should report the metric
// as unavailable rather than as zero — an empty log and an unreadable log must
// not look the same to a reviewer).
func (c *ActiveWalletCounter) Count() (int, time.Time) {
	if c == nil || c.logPath == "" {
		return 0, time.Time{}
	}
	c.mu.Lock()
	fresh := !c.computed.IsZero() && c.now().Sub(c.computed) < activeWalletTTL
	if fresh {
		n, at := c.count, c.computed
		c.mu.Unlock()
		return n, at
	}
	if c.inFlight != nil { // another goroutine is walking; wait for its result
		wg := c.inFlight
		c.mu.Unlock()
		wg.Wait()
		c.mu.Lock()
		n, at := c.count, c.computed
		c.mu.Unlock()
		return n, at
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.inFlight = wg
	c.mu.Unlock()

	n, err := c.walk()

	c.mu.Lock()
	if err == nil {
		c.count, c.computed = n, c.now()
	}
	c.inFlight = nil
	res, at := c.count, c.computed
	c.mu.Unlock()
	wg.Done()
	return res, at
}

// walk reads the live log and its rotated siblings newest-first, stopping once a
// whole file predates the window.
func (c *ActiveWalletCounter) walk() (int, error) {
	cutoff := c.now().Add(-ActiveWalletWindow)
	// Per-wallet request tallies, not a set: a wallet only counts once it reaches
	// ActiveWalletMinRequests, and its requests may be spread across rotated files.
	tally := make(map[string]int)
	var firstErr error

	for _, path := range rotatedLogPaths(c.logPath) {
		newest, err := scanWalletsSince(path, cutoff, tally)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		// Rotated files are strictly older than the ones already read; once a file's
		// newest record is before the cutoff, everything beyond it is too.
		if !newest.IsZero() && newest.Before(cutoff) {
			break
		}
	}
	if firstErr != nil && len(tally) == 0 {
		return 0, firstErr
	}
	n := 0
	for _, c := range tally {
		if c >= ActiveWalletMinRequests {
			n++
		}
	}
	return n, nil
}

// rotatedLogPaths returns the live log followed by its rotated siblings in
// newest-first order (`x.jsonl`, `x.jsonl.1`, `x.jsonl.2`, …).
func rotatedLogPaths(live string) []string {
	out := []string{live}
	matches, err := filepath.Glob(live + ".*")
	if err != nil {
		return out
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i] < matches[j] })
	out = append(out, matches...)
	return out
}

// scanWalletsSince tallies successful billable records at/after cutoff per
// wallet, and reports the newest timestamp seen in the file.
func scanWalletsSince(path string, cutoff time.Time, tally map[string]int) (newest time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Timestamp time.Time `json:"timestamp"`
			Wallet    string    `json:"wallet"`
			Status    int       `json:"status"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Timestamp.After(newest) {
			newest = rec.Timestamp
		}
		if rec.Status != 200 || rec.Wallet == "" || rec.Timestamp.Before(cutoff) {
			continue
		}
		// Wallets are EVM addresses whose casing varies by source (checksummed in
		// registration, lowercase in some clients); fold so one developer is not
		// counted twice.
		tally[strings.ToLower(rec.Wallet)]++
	}
	return newest, sc.Err()
}
