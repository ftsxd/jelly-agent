package record

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func scope(session string) Scope {
	return Scope{AppName: "jelly", UserID: "u", SessionID: session}
}

func rec(session, call string, payload []byte) Record {
	return Record{
		Scope: scope(session), InvocationID: "inv1", CallID: call,
		Label: "e1", Tool: "list_alert_rules", At: time.Now(),
		Payload: payload,
	}
}

// The acceptance loop: an oversized delivery is stored, the process goes away,
// and the part the prompt never saw is still readable afterwards.
//
// Reopening the file is the whole point — anything that only worked while the
// original handle was alive would not survive a restart, which is exactly when
// someone goes looking for what was cut.
func TestDeliverySurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	big := []byte(strings.Repeat("x", 40_000) + "TAIL")

	first := open(t, path)
	if err := first.Put(t.Context(), rec("s1", "c1", big)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A different Store over the same file, as a restarted process would have.
	again := open(t, path)
	var got []byte
	for offset := 0; ; {
		c, err := again.Read(t.Context(), scope("s1"), "c1", offset, 8192)
		if err != nil {
			t.Fatal(err)
		}
		if c.Total != len(big) {
			t.Fatalf("total = %d, want %d", c.Total, len(big))
		}
		got = append(got, c.Data...)
		offset = c.Offset + len(c.Data)
		if offset >= c.Total {
			break
		}
	}
	if !bytes.Equal(got, big) {
		t.Errorf("recovered %d bytes, want %d", len(got), len(big))
	}
	if !bytes.HasSuffix(got, []byte("TAIL")) {
		t.Error("the tail — the part a prompt would have cut — did not come back")
	}
}

// A window is a window. Storing everything is not a licence to return
// everything: the payload that was too large for a prompt is still too large
// for one response.
func TestReadIsWindowedAndBounded(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.Put(t.Context(), rec("s1", "c1", []byte(strings.Repeat("y", 100)))); err != nil {
		t.Fatal(err)
	}
	c, err := s.Read(t.Context(), scope("s1"), "c1", 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	if c.Offset != 10 || len(c.Data) != 20 {
		t.Errorf("offset=%d len=%d, want 10 and 20", c.Offset, len(c.Data))
	}
	if c.Total != 100 {
		t.Errorf("total = %d, want the full length 100", c.Total)
	}
	// A limit above the ceiling is clamped rather than honoured.
	c, err = s.Read(t.Context(), scope("s1"), "c1", 0, MaxWindow*10)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Data) > MaxWindow {
		t.Errorf("returned %d bytes, over the %d ceiling", len(c.Data), MaxWindow)
	}
}

// Scope is the permission boundary, and it is enforced in the query rather
// than after it: a call id from one conversation must not reach another's
// payload.
func TestAnotherSessionCannotRead(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.Put(t.Context(), rec("s1", "c1", []byte("secret"))); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []Scope{
		scope("s2"),
		{AppName: "jelly", UserID: "other", SessionID: "s1"},
		{AppName: "other", UserID: "u", SessionID: "s1"},
	} {
		if _, err := s.Read(t.Context(), sc, "c1", 0, 100); !errors.Is(err, ErrNotFound) {
			t.Errorf("scope %+v got err %v, want ErrNotFound", sc, err)
		}
	}
}

// A missing record and a record in someone else's scope answer the same way.
// Distinguishing them would tell a caller which sessions exist.
func TestUnknownCallIsNotFound(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Read(t.Context(), scope("s1"), "nope", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// What the tool said about truncating its own output travels with the record,
// and silence is recorded as silence.
func TestUpstreamTruncationIsThreeState(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, tc := range []struct {
		call string
		set  Upstream
		want Upstream
	}{
		{"c1", UpstreamYes, UpstreamYes},
		{"c2", UpstreamNo, UpstreamNo},
		{"c3", "", UpstreamUnknown}, // unset must not become "no"
	} {
		r := rec("s1", tc.call, []byte("x"))
		r.Upstream = tc.set
		if err := s.Put(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		c, err := s.Read(t.Context(), scope("s1"), tc.call, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if c.Upstream != tc.want {
			t.Errorf("%s: upstream = %q, want %q", tc.call, c.Upstream, tc.want)
		}
	}
}

// The checksum lets a caller reassembling several windows tell whether they
// belong to one payload.
func TestChecksumCoversTheWholePayload(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.Put(t.Context(), rec("s1", "c1", []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Read(t.Context(), scope("s1"), "c1", 0, 2)
	b, _ := s.Read(t.Context(), scope("s1"), "c1", 2, 3)
	if a.SHA256 == "" || a.SHA256 != b.SHA256 {
		t.Errorf("checksums differ across windows: %q vs %q", a.SHA256, b.SHA256)
	}
}

// An approved tool re-executes under the same call id, and the second delivery
// is the real one.
func TestRestoringTheSameCallReplacesIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.Put(t.Context(), rec("s1", "c1", []byte("placeholder"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), rec("s1", "c1", []byte("the real result"))); err != nil {
		t.Fatal(err)
	}
	c, err := s.Read(t.Context(), scope("s1"), "c1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "the real result" {
		t.Errorf("got %q, want the later delivery", c.Data)
	}
}

// A record with no session or no call id has no addressable identity, so it
// must be refused rather than written somewhere unreachable.
func TestPutRefusesAnUnaddressableRecord(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, r := range []Record{
		{Scope: scope(""), CallID: "c1"},
		{Scope: scope("s1"), CallID: ""},
	} {
		if err := s.Put(t.Context(), r); err == nil {
			t.Errorf("Put(%+v) succeeded; it cannot be read back", r.Scope)
		}
	}
}

func TestDeleteRemovesASession(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.Put(t.Context(), rec("s1", "c1", []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), scope("s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(t.Context(), scope("s1"), "c1", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after delete", err)
	}
}
