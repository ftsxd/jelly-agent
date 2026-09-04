package selector

import (
	"strings"
	"unicode"
)

// tokenize turns text into comparable units.
//
// Two kinds, because the text is two kinds.
//
// Latin runs are the high-signal ones: an operations question names its
// subject in ASCII even when the sentence around it is Chinese — pod, mysql,
// k8s, nginx, fetch_url. Those are also exactly the words a tool's own name
// and tags are built from, so they match without any cleverness.
//
// Chinese has no spaces, so a whitespace tokenizer sees one enormous token and
// matches nothing. Character bigrams are the approximation that needs no
// dictionary and no segmenter binary: 内存 falls out of "内存占用高" and 日志
// out of "看下日志". Bigrams over-generate (占用 and 用高 are both produced
// from the same three characters), which costs a little precision and buys not
// shipping a segmentation model — the right trade for a ranking signal that is
// only ever used to order candidates, never to exclude one outright.
//
// Digits are dropped: a port number or a pod ordinal matches every tool that
// mentions any number, which is noise.
func tokenize(text string) map[string]bool {
	out := map[string]bool{}
	if text == "" {
		return out
	}
	var latin []rune
	var cjk []rune

	flushLatin := func() {
		if len(latin) > 1 { // single letters carry no signal
			out[string(latin)] = true
		}
		latin = latin[:0]
	}
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i++ {
			out[string(cjk[i:i+2])] = true
		}
		// A lone character is kept only when it stands alone, where it is all
		// there is to match on.
		if len(cjk) == 1 {
			out[string(cjk)] = true
		}
		cjk = cjk[:0]
	}

	for _, r := range strings.ToLower(text) {
		switch {
		case isCJK(r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			latin = append(latin, r)
		default:
			// Underscore and hyphen split rather than join: fetch_url should
			// match a question about fetching a url, and get-pods one about
			// pods.
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()

	for t := range out {
		if isAllDigits(t) {
			delete(out, t)
		}
	}
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// overlap counts how many of a field's tokens the query also has.
//
// Counted rather than scored by ratio: a long description that happens to
// contain the query word is as relevant as a short one that does, and dividing
// by length would penalise a tool for being well documented.
func overlap(query, field map[string]bool) int {
	n := 0
	for t := range field {
		if query[t] {
			n++
		}
	}
	return n
}
