package tool

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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
