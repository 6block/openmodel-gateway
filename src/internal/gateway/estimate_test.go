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

// Vision requests carry the image as billable visual prompt tokens. The estimator
// must reserve for them (imagePromptTokenEstimate per image part) and must still
// count the text parts of a multimodal content array; the text-only heuristic
// used to see a non-string content and count nothing.
func TestEstimatePromptTokens_MultimodalContent(t *testing.T) {
	text := `{"messages":[{"role":"user","content":[{"type":"text","text":"aaaaaaaa"}]}]}`
	if got := estimatePromptTokens([]byte(text)); got != 2 {
		t.Fatalf("text-only array: want 2 (8 chars/4), got %d", got)
	}
	oneImage := `{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"aaaaaaaa"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if got := estimatePromptTokens([]byte(oneImage)); got != 2+imagePromptTokenEstimate {
		t.Fatalf("one image: want %d, got %d", 2+imagePromptTokenEstimate, got)
	}
	// The image payload itself must NOT be counted as text: a multi-megabyte data
	// URI would otherwise inflate the reservation by ~250k tokens per MiB.
	big := make([]byte, 100000)
	for i := range big {
		big[i] = 'A'
	}
	bigImage := `{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + string(big) + `"}}]}]}`
	if got := estimatePromptTokens([]byte(bigImage)); got != imagePromptTokenEstimate {
		t.Fatalf("payload bytes leaked into estimate: want %d, got %d", imagePromptTokenEstimate, got)
	}
	twoImages := `{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"x"}},{"type":"image","url":"y"}]}]}`
	if got := estimatePromptTokens([]byte(twoImages)); got != 2*imagePromptTokenEstimate {
		t.Fatalf("two images (both spellings): want %d, got %d", 2*imagePromptTokenEstimate, got)
	}
	// Plain-string content is unchanged by the multimodal branch.
	if got := estimatePromptTokens([]byte(`{"messages":[{"role":"user","content":"aaaaaaaa"}]}`)); got != 2 {
		t.Fatalf("string content regression: want 2, got %d", got)
	}
}
