package tool

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/record"
)

func scope() record.Scope {
	return record.Scope{AppName: "jelly", UserID: "u", SessionID: "s1"}
}

// The acceptance loop, through the entry point the model actually uses.
//
// Storing happens in one Store, reading in another over the same file — which
// is what a restart looks like from here, and the reason it works is that
// resolution is a query keyed by scope and handle rather than anything held in
// memory. A handle minted from a process-local counter would have failed this.
func TestModelCanReadBackAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	full := strings.Repeat("head", 500) + "MIDDLE" + strings.Repeat("tail", 500)

	before, err := record.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := before.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "list_alert_rules", At: time.Now(), Payload: []byte(full),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := record.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { after.Close() })

	// Page through with the tool, following its own next_offset — the way a
	// model would.
	var got strings.Builder
	offset := 0
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not terminate")
		}
		out, err := readResult(t.Context(), after, scope(), ReadResultArgs{
			Ref: ref, Offset: offset, Limit: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Total != len(full) {
			t.Fatalf("total = %d, want %d", out.Total, len(full))
		}
		got.WriteString(out.Data)
		if !out.HasMore {
			break
		}
		if out.NextOffset <= offset {
			t.Fatalf("next_offset %d did not advance past %d", out.NextOffset, offset)
		}
		offset = out.NextOffset
	}
	if got.String() != full {
		t.Errorf("recovered %d bytes, want %d", got.Len(), len(full))
	}
	if !strings.Contains(got.String(), "MIDDLE") {
		t.Error("the middle — what a prompt would have cut — did not come back")
	}
}

// A model reading further has to know it can. Unlike the gateway's own
// ceiling, where a smaller page hits the same limit and retrying is futile,
// paging here genuinely works — and after learning the first lesson a model
// will not try unless told.
func TestPartialReadSaysHowToContinue(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "t", At: time.Now(), Payload: []byte(strings.Repeat("x", 5000)),
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := readResult(t.Context(), s, scope(), ReadResultArgs{Ref: ref, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !out.HasMore || out.NextOffset != 100 {
		t.Fatalf("has_more=%v next_offset=%d, want true and 100", out.HasMore, out.NextOffset)
	}
	if !strings.Contains(out.Note, "offset=100") {
		t.Errorf("note = %q, want it to name the offset to continue from", out.Note)
	}
	if !strings.Contains(out.Note, "5000") {
		t.Errorf("note = %q, want it to state the total", out.Note)
	}

	// The last page must not claim there is more, or a model pages forever.
	last, err := readResult(t.Context(), s, scope(), ReadResultArgs{Ref: ref, Offset: 4900, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if last.HasMore {
		t.Error("the final page still reports more")
	}
}

// An unresolvable handle must say so. Returning an empty answer is how a model
// concludes "the tool found nothing" and reasons from it.
func TestUnresolvableHandleExplainsItself(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	_, err = readResult(t.Context(), s, scope(), ReadResultArgs{Ref: "e99"})
	if err == nil {
		t.Fatal("a missing handle returned success")
	}
	if !strings.Contains(err.Error(), "retrievable") {
		t.Errorf("err = %q, want it to point at why a result may be unreadable", err)
	}

	// And an empty ref is a caller mistake, said plainly.
	if _, err := readResult(t.Context(), s, scope(), ReadResultArgs{}); err == nil {
		t.Error("an empty ref returned success")
	}
}

// A handle from another conversation must not resolve. The scope comes from
// the invocation, not from anything the model writes, so this is the boundary
// that keeps one conversation out of another's data.
func TestHandleFromAnotherSessionDoesNotResolve(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "t", At: time.Now(), Payload: []byte("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}

	other := record.Scope{AppName: "jelly", UserID: "u", SessionID: "s2"}
	if _, err := readResult(t.Context(), s, other, ReadResultArgs{Ref: ref}); err == nil {
		t.Error("another session resolved the handle")
	}
}

// When the tool had already cut its own output, saying so is the difference
// between "here is everything" and "here is everything we were given".
func TestUpstreamTruncationIsReported(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "fetch_url", At: time.Now(), Payload: []byte("page text"),
		Upstream: record.UpstreamYes,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := readResult(t.Context(), s, scope(), ReadResultArgs{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if out.UpstreamTruncated != "yes" {
		t.Errorf("upstream_truncated = %q, want yes", out.UpstreamTruncated)
	}
	if !strings.Contains(out.Note, "已自行截断") {
		t.Errorf("note = %q, want it to warn that this is not the upstream original", out.Note)
	}
}

// The store's own readers must not be stored. Their output already came out of
// the store, so a copy buys nothing and would hand out handles to reads of
// reads. Asserted here on the names, because the exemption in the engine keys
// off them and a rename would silently reopen the loop.
func TestReaderNamesMatchTheirTools(t *testing.T) {
	store, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sc := func(adktool.Context) record.Scope { return scope() }

	rr, err := NewReadResultTool(store, sc)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Name() != ReadResultName {
		t.Errorf("read tool is named %q but the exemption uses %q", rr.Name(), ReadResultName)
	}
	sr, err := NewSearchResultTool(store, sc)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Name() != SearchResultName {
		t.Errorf("search tool is named %q but the exemption uses %q", sr.Name(), SearchResultName)
	}
}

// A first cheap read must report the whole payload's scale, so locating
// something does not require a separate tool or a full pass.
func TestASmallReadStillReportsTheScale(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now(),
		Payload: []byte(strings.Repeat("line\n", 1000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := readResult(t.Context(), s, scope(), ReadResultArgs{Ref: ref, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 5000 {
		t.Errorf("total_bytes = %d, want 5000", out.Total)
	}
	if out.Lines != 1000 {
		t.Errorf("total_lines = %d, want 1000 — the scale of the whole payload", out.Lines)
	}
	if len(out.Data) != 10 {
		t.Errorf("returned %d bytes, want the requested 10", len(out.Data))
	}
}

// The loop step 3 exists for: locate, then read only the neighbourhood.
//
// Reading a large result page by page works and costs a page of tokens per
// page. This asserts the cheaper path end to end — search reports where the
// interesting line is and how many matched, and one targeted read lands on it
// — because "the model could search" is only true if the offsets search hands
// back are usable by read.
func TestSearchThenReadFindsTheLineWithoutReadingEverything(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// One needle late in a haystack, plus a recurring line to count.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		if i == 4200 {
			b.WriteString("FATAL disk quota exceeded on node-7\n")
		} else if i%100 == 0 {
			b.WriteString("WARN retry\n")
		} else {
			b.WriteString("INFO ok\n")
		}
	}
	payload := b.String()

	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now(), Payload: []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}

	hit, err := searchResult(t.Context(), s, scope(), SearchResultArgs{
		Ref: ref, Pattern: "FATAL", Context: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hit.Total != 1 || !hit.Exact {
		t.Fatalf("FATAL matches: total=%d exact=%v, want 1/true", hit.Total, hit.Exact)
	}
	if len(hit.Hits) != 1 || hit.Hits[0].Line != 4201 {
		t.Fatalf("hits=%+v, want one at line 4201", hit.Hits)
	}
	if len(hit.Hits[0].Before) != 2 || len(hit.Hits[0].After) != 2 {
		t.Errorf("context = %d before / %d after, want 2/2",
			len(hit.Hits[0].Before), len(hit.Hits[0].After))
	}
	if hit.Lines != 5000 {
		t.Errorf("lines = %d, want 5000", hit.Lines)
	}

	// A count large enough to matter is reported exactly, so the model can
	// decide to narrow rather than guess whether it saw everything.
	warn, err := searchResult(t.Context(), s, scope(), SearchResultArgs{
		Ref: ref, Pattern: "^WARN", Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 49, not 50: line 4200 is a multiple of 100 and the FATAL line took it.
	if warn.Total != 49 || !warn.Exact {
		t.Errorf("WARN matches: total=%d exact=%v, want 49/true", warn.Total, warn.Exact)
	}
	if len(warn.Hits) != 5 || !warn.Truncated {
		t.Errorf("returned %d hits truncated=%v, want 5/true", len(warn.Hits), warn.Truncated)
	}

	// Now read only around the hit, and assert that is far less than the whole
	// payload — the entire point of searching first.
	offset := strings.Index(payload, "FATAL") - 200
	out, err := readResult(t.Context(), s, scope(), ReadResultArgs{
		Ref: ref, Offset: offset, Limit: 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Data, "FATAL disk quota exceeded on node-7") {
		t.Error("the targeted read did not contain the located line")
	}
	if len(out.Data) >= len(payload)/10 {
		t.Errorf("read %d of %d bytes — searching first bought nothing",
			len(out.Data), len(payload))
	}
	if out.Total != len(payload) || out.Lines != 5000 {
		t.Errorf("read reports %d bytes / %d lines, want %d/5000",
			out.Total, out.Lines, len(payload))
	}
}

// A pattern the model got wrong has to come back as a fixable complaint.
// Silently treating it as a literal would make the search quietly return
// nothing, and "no matches" is a conclusion the model will act on.
func TestBadPatternIsReportedNotSwallowed(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now(), Payload: []byte("a(b\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searchResult(t.Context(), s, scope(), SearchResultArgs{
		Ref: ref, Pattern: "a(b",
	}); err == nil {
		t.Fatal("an unparseable pattern was accepted")
	}
}

// A handle from another conversation must not resolve through search either.
// The read path is already covered; the same boundary has to hold on every
// entry point or the weakest one defines it.
func TestSearchDoesNotCrossSessions(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now(), Payload: []byte("secret\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	other := record.Scope{AppName: "jelly", UserID: "u", SessionID: "s2"}
	if _, err := searchResult(t.Context(), s, other, SearchResultArgs{
		Ref: ref, Pattern: "secret",
	}); err == nil {
		t.Fatal("a handle resolved from a different session")
	}
}

// What the model is told when a handle has aged out.
//
// The advice has to differ from not-found. Not-found means look again — maybe
// the handle was mistyped, maybe it came from another session. Expired means
// the bytes are gone and the only route to them is re-running the tool. A
// model given the not-found wording will spend turns trying neighbouring
// handles, which is exactly the retry loop the store exists to prevent.
func TestExpiredHandleTellsTheModelToRerunTheTool(t *testing.T) {
	s, err := record.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ref, err := s.Put(t.Context(), record.Record{
		Scope: scope(), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now().Add(-30 * 24 * time.Hour),
		Payload: []byte("logs nobody kept"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Sweep(t.Context(), time.Hour); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{{
		name: "read_result",
		call: func() error {
			_, err := readResult(t.Context(), s, scope(), ReadResultArgs{Ref: ref})
			return err
		},
	}, {
		name: "search_result",
		call: func() error {
			_, err := searchResult(t.Context(), s, scope(), SearchResultArgs{Ref: ref, Pattern: "logs"})
			return err
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("an expired handle returned success")
			}
			msg := err.Error()
			if !strings.Contains(msg, "保留期") {
				t.Errorf("message does not say the result expired: %q", msg)
			}
			if !strings.Contains(msg, "重新调用") {
				t.Errorf("message does not tell the model to re-run the tool: %q", msg)
			}
			if strings.Contains(msg, "找不到对应的已保存结果") {
				t.Error("an expired handle got the not-found wording, which sends the model looking for it")
			}
		})
	}
}
