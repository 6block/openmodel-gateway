package settlement

import "unicode/utf8"

// CharacterCount is the billable quantity of a text-to-speech request: the number of
// Unicode code points in the decoded input string, with no trimming and no
// normalization.
//
// Code points (not bytes, not UTF-16 units, not grapheme clusters) because it is the
// one count every implementation computes identically without a Unicode table: Go's
// utf8.RuneCountInString and Python's len(str) agree on it by definition, so the
// worker's attested quantity and the gateway's own count cannot drift apart on an
// emoji or a combining accent. The cross-language fixture pins the edge cases.
func CharacterCount(text string) int {
	return utf8.RuneCountInString(text)
}
