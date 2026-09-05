package ops

// Reading a delivery as text rather than as its envelope.
//
// A delivery is stored exactly as the tool produced it, and for almost every
// MCP tool that means a JSON object with the whole output in one string field.
// In that form the text's newlines are the two characters backslash and n, so
// the payload is literally one line however long it is.
//
// That quietly disabled the half of the design that matters. Measured on a
// 60000-line, 4.8MB log: the overview reported "lines: 1", search matched the
// single enormous line once and clipped it to 400 characters, and a count of
// anything came back as 1. The model, unable to search, fell back to paging —
// sixteen read calls for two facts, at four to eight times the cost of the
// search it should have run once.
//
// So the readers scan the decoded text. The stored bytes are untouched: the
// store's contract is still "what the tool delivered", and this is a view over
// it. Every reader uses the same view, so offsets, totals and line counts all
// describe the same bytes — a text view for search and a raw view for read
// would hand the model an offset that means something different to each.

import (
	"encoding/json"
)

// textShare is how much of the payload one string field must account for
// before the payload is treated as an envelope around it.
//
// A tool returning several meaningful fields must not have all but the largest
// silently dropped, so this is deliberately high: at four fifths, what is
// discarded is punctuation and a key or two, not content.
const textShare = 0.8

// TextView returns the payload as the text a reader should scan.
//
// Returns the payload itself when it is not a JSON object, when no single
// string field dominates it, or when decoding fails — the raw bytes are always
// a valid answer, just a less useful one.
func TextView(payload []byte) []byte {
	if len(payload) == 0 || payload[0] != '{' {
		return payload
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return payload
	}
	best := ""
	for _, raw := range obj {
		if len(raw) == 0 || raw[0] != '"' {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if len(s) > len(best) {
			best = s
		}
	}
	if float64(len(best)) < float64(len(payload))*textShare {
		return payload
	}
	return []byte(best)
}
