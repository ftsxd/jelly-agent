// Package tokens holds the shared, tokenizer-free token estimator used for
// budgeting prompt material (core memory files, conversation history).
//
// It is deliberately approximate: an exact count would need the provider's
// tokenizer, which differs per model and would pull in a large dependency. The
// budgets it feeds are all soft — being off by 10-20% costs a little headroom,
// never correctness.
package tokens

// Estimate approximates the token count of s: CJK runes count as ~1 token
// each, other characters as ~4 per token.
func Estimate(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x9FFF: // CJK Unified Ideographs (+ Ext A)
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK Compatibility Ideographs
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK symbols & punctuation
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // fullwidth / halfwidth forms
		return true
	default:
		return false
	}
}
