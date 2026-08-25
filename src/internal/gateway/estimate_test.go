package gateway

import "testing"

func TestEstimatePromptTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		min  int // expected lower bound (heuristic ~chars/4)
		max  int // expected upper bound
	}{
		{"empty object", `{}`, 0, 0},
		{"unparseable", `not json`, 0, 0},
		{"chat single message", `{"messages":[{"role":"user","content":"aaaaaaaa"}]}`, 2, 2},                               // 8 chars / 4
		{"chat multi message", `{"messages":[{"role":"system","content":"aaaa"},{"role":"user","content":"bbbb"}]}`, 2, 2}, // 8/4
		{"completion prompt string", `{"prompt":"aaaaaaaaaaaaaaaa"}`, 4, 4},                                                // 16/4
		{"completion prompt array", `{"prompt":["aaaa","bbbb"]}`, 2, 2},                                                    // 8/4
		{"non-string content ignored", `{"messages":[{"role":"user","content":123}]}`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := estimatePromptTokens([]byte(c.body))
			if got < c.min || got > c.max {
				t.Fatalf("estimatePromptTokens(%s) = %d, want in [%d,%d]", c.body, got, c.min, c.max)
			}
		})
	}
}

// A large prompt with a tiny max_tokens must produce a materially larger reservation
// than max_tokens alone — that's the whole point of counting the prompt.
func TestEstimatePromptTokens_LargePromptDominates(t *testing.T) {
	big := make([]byte, 40000) // 40k chars ≈ 10k tokens
	for i := range big {
		big[i] = 'x'
	}
	body := `{"max_tokens":1,"messages":[{"role":"user","content":"` + string(big) + `"}]}`
	est := estimatePromptTokens([]byte(body))
	if est < 9000 {
		t.Fatalf("large prompt should estimate ~10k tokens, got %d", est)
	}
}
