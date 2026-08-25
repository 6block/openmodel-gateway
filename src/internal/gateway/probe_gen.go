package gateway

import (
	"fmt"
	"strconv"
)

// probe_gen.go — fresh probe questions generated per run (Go port of the
// high-cardinality templates in probe/generate_questions.py). Answers are computed
// here, never transcribed, so a probe is unmemorizable and looks like an ordinary
// math question.

// generate draws a STRATIFIED sample: half baseline items, half discriminating
// ones. Uniform sampling over all templates was the earlier design and it made a
// single run nearly useless — with 8 questions drawn freely, the number of
// discriminating items landed anywhere from 1 to 7, so the same 1.5B scored 0.375
// on one run and 0.750 on the next. That spread swamps the signal the floors are
// meant to read, and a floor crossed by noise bans an honest worker.
// Fixing the mix makes consecutive runs comparable, which is what a score
// compared against a threshold requires.
func (a *auditor) generate(n int) []probeQuestion {
	a.rngMu.Lock()
	defer a.rngMu.Unlock()
	out := make([]probeQuestion, 0, n)
	nDisc := n / 2
	for i := 0; i < n-nDisc; i++ {
		out = append(out, baselineTemplates[a.rng.Intn(len(baselineTemplates))](a))
	}
	for i := 0; i < nDisc; i++ {
		out = append(out, discriminatingTemplates[a.rng.Intn(len(discriminatingTemplates))](a))
	}
	a.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func numQ(prompt string, answer int) probeQuestion {
	return probeQuestion{prompt: prompt, answer: strconv.Itoa(answer), numeric: true}
}

// Templates fall into two tiers, and the MIX is what makes the score meaningful:
//
//	tier 1 (baseline) — arithmetic every real model gets right. A model failing
//	  these is returning garbage, which is the floor case the probe must catch.
//	tier 2 (discriminating) — ported from the items measured to separate a 1.5B
//	  from the 4b+ tier in probe/baselines.json: they need the problem to be
//	  UNDERSTOOD (inverse reasoning, boundary rules, counter-intuitive traps),
//	  not just computed. Weak models answer the intuitive-but-wrong number.
//
// Tier 1 alone was the original bug: on those items a 1.5B scored a median 0.875
// — indistinguishable from a 3B, and comfortably over any floor meant to gate the
// big models. A probe that everything passes gates nothing.
//
// Every template still computes its own answer from randomized parameters, so no
// item can be memorised and each run looks like an ordinary question.
var baselineTemplates = []func(*auditor) probeQuestion{
	func(a *auditor) probeQuestion { // buy then return
		p, buy, ret := a.rng.Intn(11)+2, a.rng.Intn(15)+6, a.rng.Intn(5)+1
		return numQ(fmt.Sprintf("A shopper buys %d items at $%d each, then returns %d of them for a full refund. How much do they pay in total, in dollars?", buy, p, ret), (buy-ret)*p)
	},
	func(a *auditor) probeQuestion { // percent
		bases := []int{120, 200, 240, 300, 400, 500, 60, 80}
		pcts := []int{5, 10, 15, 20, 25, 40, 75}
		nb, pc := bases[a.rng.Intn(len(bases))], pcts[a.rng.Intn(len(pcts))]
		return numQ(fmt.Sprintf("What is %d%% of %d?", pc, nb), nb*pc/100)
	},
	func(a *auditor) probeQuestion { // discount → original price
		// The discounted price must divide EXACTLY, or the question contradicts
		// its own answer: 150 at 25% off is 112.5, which integer division wrote
		// as "costs $112" while still expecting 150 — and 112/0.75 is 149.33, so
		// a worker doing the arithmetic correctly was marked wrong. Same class of
		// bug as a stored answer that does not match its prompt.
		origs := []int{50, 60, 80, 100, 120, 150, 200}
		ds := []int{10, 20, 25, 40}
		var o, d int
		for {
			o, d = origs[a.rng.Intn(len(origs))], ds[a.rng.Intn(len(ds))]
			if o*(100-d)%100 == 0 {
				break
			}
		}
		price := o * (100 - d) / 100
		return numQ(fmt.Sprintf("After a %d%% discount an item costs $%d. What was its original price, in dollars?", d, price), o)
	},
	func(a *auditor) probeQuestion { // remainder
		b := a.rng.Intn(10) + 3
		q := a.rng.Intn(19) + 2
		rem := a.rng.Intn(b-1) + 1
		return numQ(fmt.Sprintf("What is the remainder when %d is divided by %d?", b*q+rem, b), rem)
	},
	func(a *auditor) probeQuestion { // sum 1..n
		ns := []int{10, 12, 15, 17, 20, 25, 50, 100}
		nn := ns[a.rng.Intn(len(ns))]
		return numQ(fmt.Sprintf("What is the sum of all the positive integers from 1 to %d inclusive?", nn), nn*(nn+1)/2)
	},
	func(a *auditor) probeQuestion { // handshakes
		nn := a.rng.Intn(12) + 4
		return numQ(fmt.Sprintf("There are %d people at a meeting and everyone shakes hands exactly once with everyone else. How many handshakes occur in total?", nn), nn*(nn-1)/2)
	},
	func(a *auditor) probeQuestion { // arithmetic sequence next term
		start, d := a.rng.Intn(9)+1, a.rng.Intn(8)+2
		s := []int{start, start + d, start + 2*d, start + 3*d}
		return numQ(fmt.Sprintf("What is the next number in the sequence %d, %d, %d, %d?", s[0], s[1], s[2], s[3]), start+4*d)
	},
	func(a *auditor) probeQuestion { // linear equation
		m := []int{2, 3, 4, 5}[a.rng.Intn(4)]
		x := a.rng.Intn(13) + 3
		b := a.rng.Intn(20) + 1
		return numQ(fmt.Sprintf("A number is multiplied by %d and then %d is added, giving %d. What is the number?", m, b, m*x+b), x)
	},
	func(a *auditor) probeQuestion { // total cost two kinds
		p1, n1 := a.rng.Intn(10)+3, a.rng.Intn(6)+2
		p2, n2 := a.rng.Intn(8)+2, a.rng.Intn(6)+2
		return numQ(fmt.Sprintf("Someone buys %d books at $%d each and %d pens at $%d each. How much do they spend in dollars?", n1, p1, n2, p2), n1*p1+n2*p2)
	},
	func(a *auditor) probeQuestion { // change from payment
		price := a.rng.Intn(180) + 20
		paid := ((price / 100) + 1) * 100
		return numQ(fmt.Sprintf("An item costs $%d. If you pay with $%d, how much change do you receive, in dollars?", price, paid), paid-price)
	},
}

// discriminatingTemplates separate a 1.5B from the 4b+ tier: each needs the
// problem UNDERSTOOD (inverse reasoning, boundary rules, counter-intuitive
// traps), not merely computed.
var discriminatingTemplates = []func(*auditor) probeQuestion{
	func(a *auditor) probeQuestion { // INVERSE handshakes: given the count, find the people
		n := a.rng.Intn(9) + 4 // 4..12
		return numQ(fmt.Sprintf("At a meeting everyone shook hands exactly once with everyone else, and there were %d handshakes in total. How many people were at the meeting?", n*(n-1)/2), n)
	},
	func(a *auditor) probeQuestion { // average after removing one value
		cnt := []int{5, 6, 8, 10}[a.rng.Intn(4)]
		avg := (a.rng.Intn(8) + 3) * 5         // 15..50 step 5
		rem := avg + (a.rng.Intn(5)+1)*(cnt-1) // chosen so the new mean is an integer
		total := cnt * avg
		return numQ(fmt.Sprintf("The average of %d numbers is %d. If the number %d is removed from the set, what is the average of the remaining %d numbers?", cnt, avg, rem, cnt-1), (total-rem)/(cnt-1))
	},
	func(a *auditor) probeQuestion { // squares: side = sum of two sides, area asked
		x, y := a.rng.Intn(8)+3, a.rng.Intn(8)+3
		return numQ(fmt.Sprintf("One square field has area %d square meters and another has area %d square meters. A new square field has a side equal to the sum of the side lengths of those two fields. What is the area of the new field, in square meters?", x*x, y*y), (x+y)*(x+y))
	},
	func(a *auditor) probeQuestion { // leap-year rule (the /100 and /400 exceptions)
		years := []int{1900, 2000, 2100, 2200, 2400, 1800, 2300}
		yr := years[a.rng.Intn(len(years))]
		days := 28
		if yr%4 == 0 && (yr%100 != 0 || yr%400 == 0) {
			days = 29
		}
		return numQ(fmt.Sprintf("How many days are in February in the year %d?", yr), days)
	},
	func(a *auditor) probeQuestion { // doubling: half-full is the step BEFORE full, not half the time
		h := a.rng.Intn(6) + 5 // 5..10
		return numQ(fmt.Sprintf("A bacteria colony doubles in size every hour and completely fills a jar after %d hours. After how many hours was the jar half full?", h), h-1)
	},
	func(a *auditor) probeQuestion { // rate trap: the count of workers does not change
		n := []int{3, 4, 5, 6}[a.rng.Intn(4)]
		m := []int{50, 90, 100, 120}[a.rng.Intn(4)]
		return numQ(fmt.Sprintf("If %d cats catch %d mice in %d minutes, how many cats are needed to catch %d mice in %d minutes?", n, n, n, m, m), n)
	},
	func(a *auditor) probeQuestion { // permutations of distinct items
		n := a.rng.Intn(4) + 4 // 4..7
		f := 1
		for i := 2; i <= n; i++ {
			f *= i
		}
		return numQ(fmt.Sprintf("In how many different orders can %d distinct books be arranged on a shelf?", n), f)
	},
	func(a *auditor) probeQuestion { // units digit of a power (cycle detection)
		base := []int{2, 3, 7, 8}[a.rng.Intn(4)]
		exp := (a.rng.Intn(20) + 5) * 4 // multiple of 4 keeps the cycle unambiguous
		d := 1                          // units digit of base^exp, walked directly
		for i := 0; i < exp; i++ {
			d = d * base % 10
		}
		return numQ(fmt.Sprintf("What is the units digit of %d raised to the power %d?", base, exp), d)
	},
	func(a *auditor) probeQuestion { // day of week N days from now
		days := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
		start := a.rng.Intn(7)
		n := a.rng.Intn(200) + 20
		return probeQuestion{
			prompt: fmt.Sprintf("If today is %s, what day of the week will it be %d days from now?", days[start], n),
			answer: days[(start+n)%7], numeric: false,
		}
	},
	func(a *auditor) probeQuestion { // work rate: two workers combined (integer result)
		pairs := [][3]int{{6, 12, 4}, {10, 15, 6}, {12, 24, 8}, {20, 30, 12}, {4, 12, 3}}
		p := pairs[a.rng.Intn(len(pairs))]
		return numQ(fmt.Sprintf("One worker paints a room in %d hours and another in %d hours. Working together, how many hours does it take them to paint the room?", p[0], p[1]), p[2])
	},
}
