package gateway

import (
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A stored answer that is WRONG is worse than no probe at all: it punishes
// exactly the honest workers that computed the right result (the probe item set
// shipped with such a bug — a "48 apples" item whose stored answer made all six
// baseline models look wrong). Templates compute their answers inline, so this
// re-derives every generated item independently and fails on any disagreement.
func TestProbeTemplates_AnswersAreCorrect(t *testing.T) {
	au := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 16}, nil,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))

	num := func(s string) []int {
		out := []int{}
		for _, m := range regexp.MustCompile(`-?\d+`).FindAllString(s, -1) {
			v, _ := strconv.Atoi(m)
			out = append(out, v)
		}
		return out
	}
	checked := map[string]int{}

	// Draw in batches: stratification means a batch of 1 would only ever yield
	// baseline items (n/2 = 0 discriminating), never exercising tier 2.
	batch := []probeQuestion{}
	for i := 0; i < 4000; i++ {
		if len(batch) == 0 {
			batch = au.generate(16)
		}
		q := batch[0]
		batch = batch[1:]
		p := q.prompt
		n := num(p)
		want := q.answer

		switch {
		case strings.Contains(p, "handshakes in total. How many people"):
			// n(n-1)/2 = H → solve for n independently
			h := n[0]
			got := 0
			for k := 2; k <= 100; k++ {
				if k*(k-1)/2 == h {
					got = k
					break
				}
			}
			assertEq(t, "handshake-inverse", p, want, got)
			checked["handshake-inverse"]++

		case strings.Contains(p, "is removed from the set"):
			cnt, avg, rem := n[0], n[1], n[2]
			assertEq(t, "average-removal", p, want, (cnt*avg-rem)/(cnt-1))
			checked["average-removal"]++

		case strings.Contains(p, "side equal to the sum of the side lengths"):
			a1, a2 := n[0], n[1]
			s1, s2 := isqrt(a1), isqrt(a2)
			if s1*s1 != a1 || s2*s2 != a2 {
				t.Fatalf("square areas must be perfect squares: %s", p)
			}
			assertEq(t, "square-sum", p, want, (s1+s2)*(s1+s2))
			checked["square-sum"]++

		case strings.Contains(p, "days are in February in the year"):
			yr := n[0]
			exp := 28
			if yr%4 == 0 && (yr%100 != 0 || yr%400 == 0) {
				exp = 29
			}
			assertEq(t, "leap-year", p, want, exp)
			checked["leap-year"]++

		case strings.Contains(p, "half full"):
			assertEq(t, "doubling", p, want, n[0]-1)
			checked["doubling"]++

		case strings.Contains(p, "cats are needed"):
			assertEq(t, "rate-trap", p, want, n[0]) // count is invariant
			checked["rate-trap"]++

		case strings.Contains(p, "distinct books be arranged"):
			f := 1
			for k := 2; k <= n[0]; k++ {
				f *= k
			}
			assertEq(t, "permutations", p, want, f)
			checked["permutations"]++

		case strings.Contains(p, "units digit of"):
			base, exp := n[0], n[1]
			d := 1
			for k := 0; k < exp; k++ {
				d = d * base % 10
			}
			assertEq(t, "units-digit", p, want, d)
			checked["units-digit"]++

		case strings.Contains(p, "days from now"):
			days := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
			idx := -1
			for k, d := range days {
				if strings.Contains(p, "today is "+d) {
					idx = k
				}
			}
			if idx < 0 {
				t.Fatalf("could not parse start day: %s", p)
			}
			if got := days[(idx+n[0])%7]; got != want {
				t.Fatalf("day-of-week: %s\n want %s got %s", p, want, got)
			}
			checked["day-of-week"]++

		case strings.Contains(p, "Working together"):
			x, y := n[0], n[1]
			// 1/x + 1/y = 1/t  →  t = xy/(x+y); templates are chosen to divide evenly
			if x*y%(x+y) != 0 {
				t.Fatalf("work-rate pair does not yield an integer: %s", p)
			}
			assertEq(t, "work-rate", p, want, x*y/(x+y))
			checked["work-rate"]++
		}
	}

	// Every tier-2 template must actually have been exercised.
	for _, k := range []string{"handshake-inverse", "average-removal", "square-sum", "leap-year",
		"doubling", "rate-trap", "permutations", "units-digit", "day-of-week", "work-rate"} {
		if checked[k] == 0 {
			t.Errorf("template %q was never generated in 4000 draws", k)
		}
	}
}

// The stratified mix is what makes one run comparable to the next: a fixed
// half-and-half split bounds the score's spread. Free sampling let the count of
// discriminating items swing from 1 to 7 out of 8, which moved an unchanged
// model between 0.375 and 0.750 — enough to cross a floor on noise alone.
func TestProbeTemplates_StratifiedMix(t *testing.T) {
	au := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 16}, nil,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	disc := map[string]bool{
		"handshakes in total. How many people": true, "is removed from the set": true,
		"side equal to the sum of the side lengths": true, "days are in February in the year": true,
		"half full": true, "cats are needed": true, "distinct books be arranged": true,
		"units digit of": true, "days from now": true, "Working together": true,
	}
	for run := 0; run < 200; run++ {
		n := 0
		for _, q := range au.generate(16) {
			for k := range disc {
				if strings.Contains(q.prompt, k) {
					n++
					break
				}
			}
		}
		if n != 8 {
			t.Fatalf("run %d: got %d discriminating items out of 16, want exactly 8", run, n)
		}
	}
}

// Freshness: the same template must not keep emitting the same prompt, or a
// worker could memorise the answer set instead of computing it.
func TestProbeTemplates_ItemsVary(t *testing.T) {
	au := newAuditor(ProbeConfig{Enabled: true, IntervalSec: 1, NumQuestions: 50}, nil,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		for _, q := range au.generate(50) {
			seen[q.prompt] = true
		}
	}
	if len(seen) < 300 {
		t.Fatalf("only %d distinct prompts from 1000 draws — too memorisable", len(seen))
	}
}

func assertEq(t *testing.T, name, prompt, want string, got int) {
	t.Helper()
	if want != strconv.Itoa(got) {
		t.Fatalf("%s: stored answer %s but independent computation gives %d\n  %s", name, want, got, prompt)
	}
}

func isqrt(n int) int {
	r := 0
	for (r+1)*(r+1) <= n {
		r++
	}
	return r
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// The probe's request parameters must match the baseline runner that produced
// the floors. They diverged once — the gateway asked with max_tokens 512 while
// the baseline used 800 — and a verbose small model got truncated before its
// ANSWER: line, scoring 0.646 against a floor derived from its 0.825 baseline.
// It was banned for being wordy. Guard both knobs.
func TestProbeRequest_MatchesBaselineParams(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"max_tokens": 2048`) {
		t.Error("probe must ask with max_tokens 2048, same as probe/run_probe.py")
	}
	if !strings.Contains(string(src), `"temperature": 0`) {
		t.Error("probe must ask at temperature 0, same as the baseline runner")
	}
	// Qwen3-class models burn the entire budget inside a reasoning block and never
	// reach the ANSWER: line — a 32B measured 800/800 completion tokens and scored
	// zero on every item until this was added. The offline runner has --no-think;
	// the probe must send the equivalent or it measures verbosity, not capability.
	if !strings.Contains(string(src), "/no_think") {
		t.Error("probe must append /no_think, matching the baseline runner's --no-think")
	}
	// The timeout has to fit the SLOWEST model in the fleet. At 30s a
	// tensor-parallel 32B (~39s/item here) timed out on every probe and could
	// never be admitted.
	if !strings.Contains(string(src), "cfg.RequestTimeout = 120 * time.Second") {
		t.Error("probe default timeout must accommodate a large tensor-parallel model")
	}
}

// Instruction wording must rotate: a single fixed suffix is itself the tell that
// a request is a probe, and a worker that can spot a probe can answer it with a
// better model than it serves to paying traffic.
func TestProbeInstructions_Rotate(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range []string{"a", "b", "c", "abc", "xyz", "hello world", "q1", "q2", "q3", "q4"} {
		seen[pickInstruction(p)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("instructions barely vary: %d distinct across 10 prompts", len(seen))
	}
	// Stable per prompt, so baseline and live probe ask the identical question.
	if pickInstruction("same prompt") != pickInstruction("same prompt") {
		t.Fatal("instruction choice must be deterministic for a given prompt")
	}
}

// Floors must be derived from a LARGE sample of freshly generated items. A
// 60-item sample read the 3B at 0.983; 320 items read 0.950 — and the floor
// built on the optimistic number banned an honest 3B. This pins the documented
// provenance so the next person to touch the floors sees the sample size that
// produced them.
func TestProbeFloors_DocumentSampleSize(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "320 freshly generated items") {
		t.Error("floor comment must state the sample size the measurements came from")
	}
	if !strings.Contains(s, "Small samples flatter models") {
		t.Error("floor comment must keep the small-sample warning that motivated the re-measure")
	}
}
