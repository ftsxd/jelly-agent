package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func withPayload(t *testing.T, payload string) (*Store, string) {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ref, err := s.Put(t.Context(), Record{
		Scope: scope("s1"), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now(), Payload: []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, ref
}

// The count has to be the count. "4823 matches, narrow it" and "at least
// 100000 matches, this pattern is too broad" call for different next moves, so
// a total that might be either is worse than useless.
func TestTotalIsExactWhenItCanBe(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		if i%5 == 0 {
			fmt.Fprintf(&b, "line %d ERROR boom\n", i)
		} else {
			fmt.Fprintf(&b, "line %d ok\n", i)
		}
	}
	s, ref := withPayload(t, b.String())

	res, err := s.Search(t.Context(), scope("s1"), ref, "ERROR", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 100 {
		t.Errorf("total = %d, want the exact 100", res.Total)
	}
	if !res.Exact {
		t.Error("total reported as a floor although every line was counted")
	}
	if len(res.Hits) != 5 {
		t.Errorf("returned %d hits, want the requested 5", len(res.Hits))
	}
	if !res.Truncated {
		t.Error("Truncated not set although hits were left out")
	}
	// Lines and bytes describe the whole payload, so "how big is this" is
	// answered without a second call.
	if res.Lines != 500 {
		t.Errorf("lines = %d, want 500", res.Lines)
	}
}

// Returning fewer hits than matched is not the same as having matched fewer.
func TestTruncatedHitsDoNotChangeTheTotal(t *testing.T) {
	s, ref := withPayload(t, strings.Repeat("hit\n", 50))
	res, err := s.Search(t.Context(), scope("s1"), ref, "hit", SearchOpts{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 50 || len(res.Hits) != 3 {
		t.Errorf("total=%d hits=%d, want 50 and 3", res.Total, len(res.Hits))
	}
}

// Context lines are what make a hit usable. They are also where an
// implementation that keeps pointers into a growing slice goes wrong, so both
// sides are checked on a hit that is not the first.
func TestContextComesFromBothSides(t *testing.T) {
	s, ref := withPayload(t, "a\nb\nc\nTARGET\ne\nf\ng\n")
	res, err := s.Search(t.Context(), scope("s1"), ref, "TARGET", SearchOpts{Context: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(res.Hits))
	}
	h := res.Hits[0]
	if h.Line != 4 {
		t.Errorf("line = %d, want 4", h.Line)
	}
	if strings.Join(h.Before, ",") != "b,c" {
		t.Errorf("before = %v, want [b c]", h.Before)
	}
	if strings.Join(h.After, ",") != "e,f" {
		t.Errorf("after = %v, want [e f]", h.After)
	}
}

// Several hits each need their own trailing context, and they overlap in time:
// the lines after hit one are still arriving while hit two is found. An
// implementation holding pointers into res.Hits writes some of that context
// into memory nothing reads once the slice grows.
func TestEveryHitGetsItsOwnContext(t *testing.T) {
	s, ref := withPayload(t, "X1\na\nb\nX2\nc\nd\nX3\ne\nf\n")
	res, err := s.Search(t.Context(), scope("s1"), ref, `^X\d`, SearchOpts{Context: 2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(res.Hits))
	}
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e", "f"}}
	for i, h := range res.Hits {
		if strings.Join(h.After, ",") != strings.Join(want[i], ",") {
			t.Errorf("hit %d after = %v, want %v", i+1, h.After, want[i])
		}
	}
}

// A minified payload is one line as long as the whole result. It must be
// searchable, and the line it returns must not be the entire payload.
func TestOneEnormousLineIsSearchableAndClipped(t *testing.T) {
	huge := `{"list":[` + strings.Repeat(`{"id":1},`, 5000) + `{"needle":true}]}`
	s, ref := withPayload(t, huge)
	res, err := s.Search(t.Context(), scope("s1"), ref, "needle", SearchOpts{})
	if err != nil {
		t.Fatalf("a single long line failed to scan: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1", res.Total)
	}
	if n := len([]rune(res.Hits[0].Text)); n > maxLineRunes+1 {
		t.Errorf("returned a %d-rune line; it must be clipped", n)
	}
}

// A bad pattern is the model's mistake, and it can only fix it if told what
// was wrong.
func TestBadPatternExplainsItself(t *testing.T) {
	s, ref := withPayload(t, "anything\n")
	_, err := s.Search(t.Context(), scope("s1"), ref, "([unclosed", SearchOpts{})
	if err == nil {
		t.Fatal("an invalid pattern was accepted")
	}
	if !strings.Contains(err.Error(), "unclosed") {
		t.Errorf("err = %q, want it to quote the offending pattern", err)
	}
	if _, err := s.Search(t.Context(), scope("s1"), ref, "  ", SearchOpts{}); err == nil {
		t.Error("an empty pattern was accepted")
	}
}

// Scope is the boundary here too: search must not reach another
// conversation's payload, and a malformed handle must miss.
func TestSearchRespectsScopeAndHandles(t *testing.T) {
	s, ref := withPayload(t, "secret\n")
	if _, err := s.Search(t.Context(), scope("s2"), ref, "secret", SearchOpts{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another session searched it: %v", err)
	}
	if _, err := s.Search(t.Context(), scope("s1"), "e0", "secret", SearchOpts{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a malformed handle resolved: %v", err)
	}
}

// The returned-hit ceiling is enforced, not merely defaulted.
func TestHitLimitIsClamped(t *testing.T) {
	s, ref := withPayload(t, strings.Repeat("hit\n", MaxHits*3))
	res, err := s.Search(t.Context(), scope("s1"), ref, "hit", SearchOpts{Limit: MaxHits * 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) > MaxHits {
		t.Errorf("returned %d hits, over the %d ceiling", len(res.Hits), MaxHits)
	}
	if res.Total != MaxHits*3 {
		t.Errorf("total = %d, want every match counted", res.Total)
	}
}

// A cancelled context stops the scan rather than running it to the end.
func TestSearchHonoursCancellation(t *testing.T) {
	s, ref := withPayload(t, strings.Repeat("line\n", 200_000))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Search(ctx, scope("s1"), ref, "line", SearchOpts{}); err == nil {
		t.Error("a cancelled search ran to completion")
	}
}

// cancelAfter cancels part-way through the stream.
//
// The test above only proves the database refuses an already-cancelled
// context; it passes with the scan's own check deleted, which makes it a false
// green for the thing it is named after. The scan is what can run for seconds
// over a large payload, so its check needs a test that fails when it is
// removed — and that means cancelling while the scan is reading, without
// racing a timer.
type cancelAfter struct {
	r      io.Reader
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.n--; c.n == 0 {
		c.cancel()
	}
	return n, err
}

func TestScanStopsWhenCancelledMidScan(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	body := strings.Repeat("line\n", 400_000) // several reader fills
	r := &cancelAfter{r: strings.NewReader(body), n: 1, cancel: cancel}

	res, err := scanMatches(ctx, r, regexp.MustCompile("line"), DefaultHits, 0, MaxCountedHits, len(body))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan did not stop on cancellation: err=%v lines=%d", err, res.Lines)
	}
}

// The total is a promise about how many lines matched. When counting itself
// hits its ceiling that promise cannot be kept, and the honest answer is a
// floor — reported through Exact, so a caller can tell "4823 matches" from "at
// least 100000 matches, this pattern is too broad".
func TestScanReportsAFloorOnceCountingIsCapped(t *testing.T) {
	body := strings.Repeat("hit\n", 50)

	exact, err := scanMatches(t.Context(), strings.NewReader(body),
		regexp.MustCompile("hit"), 5, 0, 1000, len(body))
	if err != nil {
		t.Fatal(err)
	}
	if !exact.Exact || exact.Total != 50 {
		t.Errorf("under the cap: got total=%d exact=%v, want 50/true", exact.Total, exact.Exact)
	}

	capped, err := scanMatches(t.Context(), strings.NewReader(body),
		regexp.MustCompile("hit"), 5, 0, 10, len(body))
	if err != nil {
		t.Fatal(err)
	}
	if capped.Exact {
		t.Error("counting was capped at 10 of 50 matches but the total is still reported as exact")
	}
	if capped.Total != 10 {
		t.Errorf("floor = %d, want the cap 10", capped.Total)
	}
	// The floor is about counting, not about delivery: hits are bounded by
	// their own limit and Truncated still has to say so.
	if len(capped.Hits) != 5 || !capped.Truncated {
		t.Errorf("hits=%d truncated=%v, want 5/true", len(capped.Hits), capped.Truncated)
	}
}

// Read and Search must agree about how many lines a payload has. The same
// result reporting 1000 lines through one tool and 1001 through another is
// worse than either being slightly off: the reader cannot tell which to
// believe, and the number is there to decide how to proceed.
func TestReadAndSearchAgreeOnLineCount(t *testing.T) {
	for _, payload := range []string{
		"a\nb\nc\n", // trailing newline
		"a\nb\nc",   // no trailing newline
		"one line",  // single line, no newline
		"\n",        // just a newline
		"",          // empty
		"a\n\nb\n",  // blank line in the middle
	} {
		s, ref := withPayload(t, payload)
		chunk, err := s.ReadLabel(t.Context(), scope("s1"), ref, 0, DefaultWindow)
		if err != nil {
			t.Fatalf("%q: %v", payload, err)
		}
		// A pattern that matches every line gives Search's own count.
		res, err := s.Search(t.Context(), scope("s1"), ref, "(?s).*", SearchOpts{Limit: MaxHits})
		if err != nil {
			t.Fatalf("%q: %v", payload, err)
		}
		if chunk.Lines != res.Lines {
			t.Errorf("payload %q: read says %d lines, search says %d",
				payload, chunk.Lines, res.Lines)
		}
	}
}

// The end-to-end shape that was broken: a tool's output arrives as one JSON
// string field, so the log's newlines are stored as backslash-n.
//
// Measured on a 60000-line log before the text view existed: read reported one
// line, search matched the single enormous line once and clipped it to 400
// characters, and "how many TIMEOUTs" came back as 1 instead of 1622. Counting
// is the whole reason search reports a total, so this is the case it has to
// get right.
func TestSearchCountsLinesInsideAJSONTextEnvelope(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		if i%37 == 0 {
			b.WriteString("2026-09-04 WARN payment-api TIMEOUT upstream=db-node-3\n")
		} else {
			b.WriteString("2026-09-04 INFO payment-api request ok\n")
		}
	}
	log := b.String()
	want := strings.Count(log, "TIMEOUT")

	payload, err := json.Marshal(map[string]any{"output": log})
	if err != nil {
		t.Fatal(err)
	}
	s, ref := withPayload(t, string(payload))

	res, err := s.Search(t.Context(), scope("s1"), ref, "TIMEOUT", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != want || !res.Exact {
		t.Errorf("total = %d (exact=%v), want %d — the envelope was scanned instead of the text",
			res.Total, res.Exact, want)
	}
	if res.Lines != 3000 {
		t.Errorf("lines = %d, want 3000", res.Lines)
	}
	// A hit must be one log line, not a 400-rune slice of the whole payload.
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	if h := res.Hits[0]; !strings.Contains(h.Text, "TIMEOUT") || len(h.Text) > 200 {
		t.Errorf("first hit is %d chars: %.80q", len(h.Text), h.Text)
	}

	// Read has to agree with it, or an offset from one means something else to
	// the other.
	chunk, err := s.ReadLabel(t.Context(), scope("s1"), ref, 0, DefaultWindow)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Lines != res.Lines {
		t.Errorf("read says %d lines, search says %d", chunk.Lines, res.Lines)
	}
	if chunk.Total != len(log) {
		t.Errorf("read total = %d, want the %d-byte log rather than the %d-byte envelope",
			chunk.Total, len(log), len(payload))
	}
	if !strings.HasPrefix(string(chunk.Data), "2026-09-04 WARN") {
		t.Errorf("read returned the envelope, not the text: %.60q", chunk.Data)
	}
}
