package server

// The task centre's endpoints.
//
// Everything is read from what already exists: the session events give the
// steps, tool_calls gives the durations, tool_results gives the artifacts and
// says which of them can still be read. Nothing is stored for this view except
// which runs a caller explicitly joined to an earlier task, so nothing here
// can disagree with what actually happened.

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/logging"
	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/record"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/task"
)

// How far a list request reads back through the sessions.
//
// It reads in pages and stops as soon as it has enough tasks to answer, rather
// than folding a fixed prefix and paging inside it. The difference showed as
// soon as a filter was involved: a status the recent sessions did not contain
// returned an empty page while matching tasks sat one session further back,
// and asking for page two of anything returned nothing at all, because the
// prefix rarely held two pages' worth.
//
// taskScanMax is the ceiling that keeps a filter matching nothing from walking
// the whole history. Reaching it is reported rather than hidden: the answer
// then says its total is a floor, in the same words the result search uses.
const (
	taskScanPage = 40
	taskScanMax  = 600
)

// artifactMinBytes is the size at which a stored result is worth listing on
// its own.
//
// Every gatewayed call is stored, so "was stored" cannot be the test — it
// would put an empty listing next to a four-megabyte log and call them both
// products of the run. What earns a place is being worth opening: something
// substantial, or something the model did not get all of, which is the case
// where the store is the only route to the rest. Everything smaller is still
// there, in the step that produced it.
const artifactMinBytes = 2048

// toolInfoResolver answers what a tool is, for phase classification.
//
// Built once per request from the registry rather than looked up per call:
// the registry snapshot is a slice, and scanning it for every call would be
// quadratic in the length of the run.
func (s *Server) toolInfoResolver() func(string) ToolInfo {
	reg := s.engine().ToolRegistry()
	cache := map[string]ToolInfo{}
	return func(tool string) ToolInfo {
		if info, ok := cache[tool]; ok {
			return info
		}
		var info ToolInfo
		// Lookup, not Available: this asks what a tool IS, and Available
		// answers whether it is usable in the current incident — it filters
		// by backend, so with no incident it hides every tool that declares
		// one. Declared MCP metadata all declares a backend, so the view that
		// most needs it saw none of it.
		if reg != nil {
			if m, ok := reg.Lookup(tool); ok {
				info = ToolInfo{
					Kind:     m.Produces,
					Mutating: m.SideEffect == ops.SideEffectMutating || m.SideEffect == ops.SideEffectRisky,
					Known:    true,
				}
			}
		}
		cache[tool] = info
		return info
	}
}

// tasksOf folds one session into the tasks it contains.
func (s *Server) tasksOf(r *http.Request, svc adksession.Service, id string, infoOf func(string) ToolInfo) ([]Task, error) {
	resp, err := svc.Get(r.Context(), &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	})
	if err != nil || resp.Session == nil {
		return nil, err
	}
	var events []*adksession.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	frames, _ := projectAll(events)

	// Which runs were joined to an earlier task. Empty for everything written
	// before that was possible, which is what makes historical data fold into
	// one task per run rather than not at all.
	links, err := task.OfSession(s.engine().SessionDBPath(), id)
	if err != nil {
		links = nil
	}
	tasks := foldTasks(id, frames, infoOf, links)

	for i := range tasks {
		if st, known := s.runs().status(tasks[i].ID); known {
			tasks[i].Status = st
		}
	}
	return tasks, nil
}

// recordedRuns reports which runs of these sessions left something in the
// store, for the admission test.
//
// One query for the whole page rather than one per session. It is the only
// expensive test in that rule, and the scan reads back hundreds of sessions —
// asked one at a time it was the page's dominant cost, and the answer is a
// single distinct-select over an index either way.
//
// A failure is an empty answer rather than an error: this decides whether a
// run whose tools all failed still counts as work, and losing the whole list
// because that probe could not be made would be a worse outcome than
// occasionally filing such a run as conversation.
func (s *Server) recordedRuns(ctx context.Context, sessionIDs []string) map[string]map[string]bool {
	probe := s.recordsProbe
	if probe == nil {
		store, err := s.engine().Records()
		if err != nil || store == nil {
			return nil
		}
		probe = store.RecordedRuns
	}
	got, err := probe(ctx, engine.AppName, engine.UserID, sessionIDs)
	if err != nil {
		slog.Warn("任务列表无法确认哪些运行留下了产物，本次按没有产物处理", logging.Err(err))
		return nil
	}
	return got
}

// handleTasks lists tasks, reading back through the sessions until it has
// enough of them to answer.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	infoOf := s.toolInfoResolver()
	wantStatus := strings.TrimSpace(r.URL.Query().Get("status"))
	wantType := strings.TrimSpace(r.URL.Query().Get("type"))
	limit := queryInt(r, "limit", 50, 1, 200)
	offset := queryInt(r, "offset", 0, 0, 1<<20)
	// One past the page, so "is there more" is answered by having found it
	// rather than by guessing from a count that was itself truncated.
	need := offset + limit + 1

	var (
		// Never nil: the page iterates this, and JSON null is not iterable.
		all           = []Task{}
		skippedChat   int
		scanned       int
		sessionsTotal int
		exhausted     bool
	)
	for scanned < taskScanMax {
		metas, count, err := jellysession.ListPage(
			s.engine().SessionDBPath(), engine.AppName, engine.UserID, taskScanPage, scanned)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sessionsTotal = count
		if len(metas) == 0 {
			exhausted = true
			break
		}
		scanned += len(metas)
		ids := make([]string, 0, len(metas))
		for _, m := range metas {
			ids = append(ids, m.ID)
		}
		recorded := s.recordedRuns(r.Context(), ids)
		// The oldest update time in this page bounds everything still
		// unscanned: sessions come back newest-updated first, so no session
		// after these can have been touched later than this.
		//
		// It is a bound on session activity, not on task age, and that is the
		// whole difficulty. A session that was active this morning can hold a
		// hundred tasks from last month, and stopping as soon as it had filled
		// the page put those on top while a task from yesterday sat in the
		// very next session, unread. Having enough tasks is not the same as
		// having the right ones.
		//
		// Rounded up to the next second, because the list reports update times
		// in whole seconds while a task's start is in milliseconds. Multiplying
		// the truncated value gave a frontier up to 999ms too low — below a
		// task that genuinely sits above it — which is a bound that lets the
		// scan stop one session too early. A bound that is nearly right is not
		// a bound.
		frontier := (metas[len(metas)-1].LastUpdate + 1) * 1000
		for _, m := range metas {
			tasks, err := s.tasksOf(r, svc, m.ID, infoOf)
			if err != nil {
				continue // a session that vanished mid-scan is not an error for the list
			}
			runs := recorded[m.ID]
			for _, t := range tasks {
				// Conversation is not work. "你好" belongs in the sessions
				// view, which already shows it; putting it here makes the
				// board about messages instead of about goals.
				if !IsTask(t, func() bool { return anyRecorded(t, runs) }) {
					skippedChat++
					continue
				}
				if wantStatus != "" && t.Status != wantStatus {
					continue
				}
				if wantType != "" && t.Type != wantType {
					continue
				}
				t.Steps = summarizeSteps(t.Steps)
				t.Reply = firstLine(t.Reply)
				all = append(all, t)
			}
		}
		if scanned >= sessionsTotal {
			exhausted = true
			break
		}
		// Enough tasks is not the test. The test is that the page cannot
		// change: the last task on it must already be at least as new as
		// anything an unscanned session could still produce.
		sortTasksNewestFirst(all)
		if len(all) >= need && all[need-1].StartedAt >= frontier {
			break
		}
	}
	sortTasksNewestFirst(all)

	found := len(all)
	if offset > found {
		offset = found
	}
	end := min(offset+limit, found)

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": all[offset:end], "total": found,
		// total_exact says whether that count is the number of tasks or a
		// floor. It is a floor whenever the scan stopped early — the same
		// distinction the result search draws, and for the same reason: "42
		// tasks" and "at least 42 tasks, look further back" call for different
		// next moves.
		"total_exact": exhausted,
		"limit":       limit, "offset": offset,
		"has_more":         end < found || !exhausted,
		"scanned_sessions": scanned,
		"sessions_total":   sessionsTotal,
		// Reported rather than silently dropped, so "why is my chat not here"
		// has an answer on the page instead of in the source.
		"skipped_chat": skippedChat,
	})
}

func anyRecorded(t Task, recorded map[string]bool) bool {
	for _, run := range t.Runs {
		if recorded[run] {
			return true
		}
	}
	return false
}

// summarizeSteps keeps the shape of a task's progress without its detail.
func summarizeSteps(steps []Step) []Step {
	out := make([]Step, 0, len(steps))
	for _, s := range steps {
		s.Tools, s.Artifacts, s.Note = nil, nil, ""
		out = append(out, s)
	}
	return out
}

// handleTask returns one task with its steps, durations, artifacts and reply.
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	sessionID, round := r.PathValue("session"), r.PathValue("round")
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks, err := s.tasksOf(r, svc, sessionID, s.toolInfoResolver())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := task.ID(sessionID, round)
	var t *Task
	for i := range tasks {
		if tasks[i].ID == want {
			t = &tasks[i]
			break
		}
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}

	// Durations come from the metrics table, which is where they are measured.
	// The frames carry none on purpose: ADK merges parallel tool responses
	// into one event reusing the first one's timestamp, so a frame-to-frame
	// delta is not a duration.
	//
	// Keyed by run and call together. A merged task holds more than one run,
	// and a call id is only unique inside its own — keying by the call alone
	// gave every run's first call whichever duration was written last.
	if tr := s.engine().Metrics(); tr != nil {
		byCall := map[string]int{}
		for _, run := range t.Runs {
			rows, err := tr.ByInvocation(sessionID, run)
			if err != nil {
				continue
			}
			for _, c := range rows {
				byCall[resultKey(run, c.CallID)] = c.DurationMS
			}
		}
		for i := range t.Steps {
			for j := range t.Steps[i].Tools {
				tl := &t.Steps[i].Tools[j]
				tl.DurationMS = byCall[resultKey(tl.Round, tl.CallID)]
			}
		}
	}

	artifacts, index := s.artifactsOf(r, sessionID, t)

	// Every tool call's own result, addressable, so a step's detail can show
	// what each call returned even when the result is too small to be a
	// product of the run in its own right.
	writeJSON(w, http.StatusOK, map[string]any{
		"task": t, "artifacts": artifacts, "results": index,
	})
}

// Artifact is something the run produced that is worth opening on its own.
type Artifact struct {
	Label  string `json:"label"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // tool_result
	Step   string `json:"step,omitempty"`
	At     int64  `json:"at,omitempty"`
	Bytes  int    `json:"bytes"`
	Lines  int    `json:"lines,omitempty"`

	// Complete says the model received the whole thing.
	Complete bool `json:"complete"`
	// Retrievable is answered by the store now, not by what the model was told
	// then. Historical events predate the flag entirely, and deriving it from
	// them marked every older result "未落库" while its bytes sat in the
	// database, readable.
	Retrievable bool   `json:"retrievable"`
	Expired     bool   `json:"expired"`
	Upstream    string `json:"upstream,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

// resultKey addresses one call within one run.
//
// The store's primary key is (app, user, session, invocation, call) and this
// is the part of it that varies inside a task. Anything that indexes calls by
// the id alone silently merges two runs' first calls.
func resultKey(round, callID string) string { return round + "/" + callID }

// ResultRef is one tool call's stored result, for the step detail.
//
// Label is what a caller reads with: it is unique per session, where the call
// id is unique only per run.
type ResultRef struct {
	Label       string `json:"label"`
	CallID      string `json:"call_id"`
	Round       string `json:"round"`
	Bytes       int    `json:"bytes"`
	Retrievable bool   `json:"retrievable"`
	Expired     bool   `json:"expired"`
}

// artifactsOf lists what a run produced, without reading any of it.
//
// Returns the products worth listing separately, and an index of every stored
// result by call id so the step detail can offer the small ones too.
func (s *Server) artifactsOf(r *http.Request, sessionID string, t *Task) ([]Artifact, map[string]ResultRef) {
	stepOf, summaryOf, linesOf, completeOf := map[string]string{}, map[string]string{}, map[string]int{}, map[string]bool{}
	for _, st := range t.Steps {
		for _, ref := range st.Artifacts {
			stepOf[ref] = st.ID
		}
		for _, tl := range st.Tools {
			if tl.EvidenceID == "" {
				continue
			}
			summaryOf[tl.EvidenceID] = tl.Summary
			linesOf[tl.EvidenceID] = tl.Lines
			completeOf[tl.EvidenceID] = !tl.Truncated && !tl.Withheld
		}
	}

	out, index := []Artifact{}, map[string]ResultRef{}
	store, err := s.engine().Records()
	if err != nil {
		return out, index
	}
	runs := map[string]bool{}
	for _, run := range t.Runs {
		runs[run] = true
	}
	items, err := store.List(r.Context(), record.Scope{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID,
	}, record.ListOpts{Limit: record.MaxListLimit})
	if err != nil {
		return out, index
	}
	for _, it := range items {
		if !runs[it.InvocationID] {
			continue
		}
		// The row is here, so the bytes are here — unless retention dropped
		// them, which the row itself records.
		readable := !it.Expired
		index[resultKey(it.InvocationID, it.CallID)] = ResultRef{
			Label: it.Label, CallID: it.CallID, Round: it.InvocationID,
			Bytes: it.Bytes, Retrievable: readable, Expired: it.Expired,
		}
		if it.Bytes < artifactMinBytes && completeOf[it.Label] {
			continue // small and whole: it lives in its step, not on the shelf
		}
		out = append(out, Artifact{
			Label: it.Label, CallID: it.CallID, Name: it.Tool, Kind: "tool_result",
			Step: stepOf[it.Label], At: it.At.UnixMilli(), Bytes: it.Bytes,
			Lines: linesOf[it.Label], Complete: completeOf[it.Label],
			Retrievable: readable, Expired: it.Expired,
			Upstream: string(it.Upstream), Summary: summaryOf[it.Label],
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, index
}
