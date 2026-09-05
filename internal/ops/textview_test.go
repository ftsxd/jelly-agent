package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

// The case the view exists for, at the shape that actually broke.
//
// An MCP tool returns its whole output in one JSON string field, so the log's
// newlines are stored as the two characters backslash and n. Measured on a
// 60000-line log before this existed: the overview said "1 line", search
// matched the single enormous line once and clipped it to 400 characters, and
// a count came back as 1.
func TestATextEnvelopeIsReadAsItsText(t *testing.T) {
	log := strings.Repeat("2026-09-04T00:00:00Z WARN payment-api TIMEOUT upstream=db-node-3\n", 1000)
	payload, err := json.Marshal(map[string]any{"output": log})
	if err != nil {
		t.Fatal(err)
	}
	if got := CountLines(payload); got != 1 {
		t.Fatalf("precondition: the raw envelope should be one line, got %d", got)
	}

	view := TextView(payload)
	if string(view) != log {
		t.Errorf("view is %d bytes, want the %d-byte log", len(view), len(log))
	}
	if got := CountLines(view); got != 1000 {
		t.Errorf("lines = %d, want 1000", got)
	}
}

// A payload with several meaningful fields must not have all but the biggest
// silently dropped. Losing content is a worse failure than reading escapes.
func TestAMultiFieldPayloadIsLeftAlone(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"status": "degraded", "region": "cn-north", "owner": "payments-team",
		"note": strings.Repeat("x", 60), "detail": strings.Repeat("y", 60),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := TextView(payload); string(got) != string(payload) {
		t.Errorf("a payload with no dominant field was reduced to %q", got)
	}
}

func TestNonEnvelopesArePassedThrough(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"纯文本", "just some text\nwith lines\n"},
		{"JSON 数组", `[{"id":1},{"id":2}]`},
		{"空", ""},
		{"坏 JSON", `{"output": "unterminated`},
		{"没有字符串字段", `{"a":1,"b":[1,2,3],"c":{"d":4}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TextView([]byte(tc.in)); string(got) != tc.in {
				t.Errorf("TextView(%q) = %q, want it unchanged", tc.in, got)
			}
		})
	}
}

// Whichever field is the bulk of the payload is the one to read, regardless of
// its name — "output", "content", "text" and "stdout" are all in use, and
// keying off a name list would work until the next server.
func TestTheDominantFieldWinsWhateverItIsCalled(t *testing.T) {
	body := strings.Repeat("line\n", 500)
	for _, key := range []string{"output", "content", "text", "stdout", "结果"} {
		payload, err := json.Marshal(map[string]any{key: body, "truncated": false})
		if err != nil {
			t.Fatal(err)
		}
		if got := TextView(payload); string(got) != body {
			t.Errorf("%s: view is %d bytes, want %d", key, len(got), len(body))
		}
	}
}
