package metrics

import (
	"fmt"
	"sort"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// ToolLatency is one tool's timing and failure breakdown over the queried
// window.
//
// Calls counts only rows this package recorded, which is not the same as every
// call the agent has ever made: sessions that predate the telemetry hooks have
// events but no rows. Callers that also show an all-history count must say
// which number came from where, or the two will look like a contradiction.
type ToolLatency struct {
	Tool     string         `json:"tool"`
	Calls    int            `json:"calls"`
	OK       int            `json:"ok"`
	P50MS    int            `json:"p50_ms"`
	P95MS    int            `json:"p95_ms"`
	MaxMS    int            `json:"max_ms"`
	ErrKinds map[string]int `json:"err_kinds,omitempty"` // cause bucket -> count
}

// Summary is the recorded picture over a window.
type Summary struct {
	Since time.Time     `json:"since"` // timestamp of the oldest row counted
	Calls int           `json:"calls"`
	Tools []ToolLatency `json:"tools"`
}

// Summary aggregates recorded calls at or after since. A zero since covers
// everything.
//
// Percentiles are computed here rather than in SQL because SQLite has no
// percentile function, and the honest alternatives (a window-function query, or
// storing pre-bucketed histograms) both cost more than sorting a few thousand
// integers. If this table ever grows past the point where that is true, the fix
// is a rollup table, not cleverer SQL.
func (r *Recorder) Summary(since time.Time) (*Summary, error) {
	if r == nil || r.db == nil {
		return &Summary{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	const q = `SELECT tool, duration_ms, ok, err_kind, at
	           FROM tool_calls WHERE at >= ? ORDER BY tool`
	rows, err := r.db.Query(q, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("metrics: summary: %w", err)
	}
	defer rows.Close()

	type acc struct {
		durations []int
		ok        int
		errKinds  map[string]int
	}
	byTool := map[string]*acc{}
	out := &Summary{}

	for rows.Next() {
		var tool, errKind, at string
		var dur int
		var ok bool
		if err := rows.Scan(&tool, &dur, &ok, &errKind, &at); err != nil {
			return nil, fmt.Errorf("metrics: summary scan: %w", err)
		}
		a := byTool[tool]
		if a == nil {
			a = &acc{errKinds: map[string]int{}}
			byTool[tool] = a
		}
		a.durations = append(a.durations, dur)
		if ok {
			a.ok++
		} else if errKind != "" {
			a.errKinds[errKind]++
		}
		out.Calls++
		if ts, err := time.Parse(time.RFC3339Nano, at); err == nil {
			if out.Since.IsZero() || ts.Before(out.Since) {
				out.Since = ts
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: summary rows: %w", err)
	}

	for tool, a := range byTool {
		sort.Ints(a.durations)
		t := ToolLatency{
			Tool:  tool,
			Calls: len(a.durations),
			OK:    a.ok,
			P50MS: percentile(a.durations, 0.50),
			P95MS: percentile(a.durations, 0.95),
			MaxMS: a.durations[len(a.durations)-1],
		}
		if len(a.errKinds) > 0 {
			t.ErrKinds = a.errKinds
		}
		out.Tools = append(out.Tools, t)
	}
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Calls != out.Tools[j].Calls {
			return out.Tools[i].Calls > out.Tools[j].Calls
		}
		return out.Tools[i].Tool < out.Tools[j].Tool
	})
	return out, nil
}

// Summary is the tracker-level accessor, so callers hold one handle rather
// than reaching past it for the store.
func (t *Tracker) Summary(since time.Time) (*Summary, error) {
	if t == nil {
		return &Summary{}, nil
	}
	return t.rec.Summary(since)
}

// percentile returns the nearest-rank percentile of a sorted slice: the
// smallest value at or above the requested share of the samples. With three
// samples, p95 is the largest — deliberately, because a tail statistic that
// smooths away the only slow call in a small sample hides exactly what it is
// there to show.
func percentile(sorted []int, p float64) int {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(float64(n)*p + 0.999999) // ceil without importing math
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// CallRow is one recorded tool call, as it happened.
//
// Distinct from the aggregate above: Summary answers "how is this tool doing
// across the fleet", this answers "what did this particular run do". The
// duration is the reason it exists — it is measured at the gateway and stored
// here, and it is the only place it survives. The event stream deliberately
// carries no elapsed time, because ADK merges parallel tool responses into one
// event reusing the first one's timestamp, so a frame-to-frame delta is not a
// duration and reporting it as one would be worse than reporting nothing.
type CallRow struct {
	At          time.Time `json:"at"`
	CallID      string    `json:"call_id"`
	Agent       string    `json:"agent,omitempty"`
	Tool        string    `json:"tool"`
	DurationMS  int       `json:"duration_ms"`
	OK          bool      `json:"ok"`
	ErrKind     string    `json:"err_kind,omitempty"`
	Err         string    `json:"err,omitempty"`
	ResultBytes int       `json:"result_bytes"`
	EvidenceID  string    `json:"evidence_id,omitempty"`
	Retrievable bool      `json:"retrievable"`
}

// ByInvocation returns the calls of one run, oldest first.
//
// invocationID may be empty, which returns the whole session — useful for a
// session that predates invocation ids being recorded.
//
// Uses idx_tool_calls_session, which has existed since the table was created
// and has had no reader until now.
func (r *Recorder) ByInvocation(sessionID, invocationID string) ([]CallRow, error) {
	if r == nil || r.db == nil || sessionID == "" {
		return nil, nil
	}
	where := `WHERE session_id = ?`
	args := []any{sessionID}
	if invocationID != "" {
		where += ` AND invocation_id = ?`
		args = append(args, invocationID)
	}

	rows, err := r.db.Query(`
		SELECT at, call_id, agent, tool, duration_ms, ok, err_kind, err,
		       result_bytes, evidence_id, retrievable
		FROM tool_calls `+where+` ORDER BY id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("metrics: calls of %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []CallRow
	for rows.Next() {
		var (
			c        CallRow
			at       string
			ok, retr bool
		)
		if err := rows.Scan(&at, &c.CallID, &c.Agent, &c.Tool, &c.DurationMS, &ok,
			&c.ErrKind, &c.Err, &c.ResultBytes, &c.EvidenceID, &retr); err != nil {
			return nil, fmt.Errorf("metrics: scan call: %w", err)
		}
		c.At, _ = time.Parse(time.RFC3339Nano, at)
		c.OK, c.Retrievable = ok, retr
		out = append(out, c)
	}
	return out, rows.Err()
}

// ByInvocation passes through to the recorder, so callers holding a Tracker
// (which is what the engine hands out) do not need the recorder as well.
func (t *Tracker) ByInvocation(sessionID, invocationID string) ([]CallRow, error) {
	if t == nil || t.rec == nil {
		return nil, nil
	}
	return t.rec.ByInvocation(sessionID, invocationID)
}

// DeleteSessions removes the call rows of the given sessions.
//
// Called when the sessions themselves are deleted. Timing rows are not
// incidental bookkeeping: they carry the tool name, the arguments' shape and
// the evidence handle, so leaving them behind leaves a readable trace of a
// conversation the user asked to be gone.
func (r *Recorder) DeleteSessions(ids []string) (int, error) {
	if r == nil || r.db == nil || len(ids) == 0 {
		return 0, nil
	}
	n := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		res, err := r.db.Exec(`DELETE FROM tool_calls WHERE session_id = ?`, id)
		if err != nil {
			// A fresh database has no table yet, which means no rows to
			// remove rather than a failure.
			if storage.IsMissingTable(err) {
				return n, nil
			}
			return n, fmt.Errorf("metrics: delete calls of %s: %w", id, err)
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n += int(c)
		}
	}
	return n, nil
}

// DeleteSessions passes through to the recorder, so callers holding a Tracker
// (which is what the engine hands out) do not need the recorder as well.
func (t *Tracker) DeleteSessions(ids []string) (int, error) {
	if t == nil || t.rec == nil {
		return 0, nil
	}
	return t.rec.DeleteSessions(ids)
}
