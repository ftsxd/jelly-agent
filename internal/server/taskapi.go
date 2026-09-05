package server

// The task centre's endpoints.
//
// All three read from what already exists: the session events give the steps,
// tool_calls gives the durations, tool_results gives the artifacts. Nothing is
// stored for this view, so nothing here can disagree with what actually
// happened — the events are ADK's own record and the only one that is
// guaranteed to be there.

import (
	"net/http"
	"sort"
	"strings"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/record"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
)

// taskListScan bounds how many sessions a list request folds.
//
// The list has to project sessions to find their tasks, so an unbounded scan
// would make the page slower the longer the deployment has been running. This
// caps the work; older tasks are reachable through their session.
const taskListScan = 60

// kindResolver answers what a tool produces, for step grouping.
//
// Built once per request from the registry rather than per tool, because the
// registry snapshot is a slice and looking a name up in it repeatedly would be
// quadratic in the number of calls.
func (s *Server) kindResolver() func(string) ops.EvidenceKind {
	byName := map[string]ops.EvidenceKind{}
	if reg := s.engine().ToolRegistry(); reg != nil {
		for _, m := range reg.Available(nil) {
			byName[m.Name] = m.Produces
		}
	}
	return func(tool string) ops.EvidenceKind { return byName[tool] }
}

// tasksOf folds one session into its tasks, newest first.
func (s *Server) tasksOf(r *http.Request, svc adksession.Service, id string, kindOf func(string) ops.EvidenceKind) ([]Task, error) {
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
	tasks := foldTasks(id, frames, kindOf)

	// Live status is the one thing the events cannot say — see runs.go.
	for i := range tasks {
		if st, known := s.runs().status(tasks[i].ID); known {
			tasks[i].Status = st
		}
	}
	return tasks, nil
}

// handleTasks lists tasks across recent sessions.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	metas, _, err := jellysession.ListPage(s.engine().SessionDBPath(), engine.AppName, engine.UserID, taskListScan, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	kindOf := s.kindResolver()
	wantStatus := strings.TrimSpace(r.URL.Query().Get("status"))
	wantType := strings.TrimSpace(r.URL.Query().Get("type"))

	all := make([]Task, 0, len(metas))
	for _, m := range metas {
		tasks, err := s.tasksOf(r, svc, m.ID, kindOf)
		if err != nil {
			continue // a session that vanished mid-scan is not an error for the list
		}
		for _, t := range tasks {
			if wantStatus != "" && t.Status != wantStatus {
				continue
			}
			if wantType != "" && t.Type != wantType {
				continue
			}
			// The list does not need the steps' innards, and sending them
			// would make it many times larger than the page it draws.
			t.Steps = summarizeSteps(t.Steps)
			t.Answer = firstLine(t.Answer)
			all = append(all, t)
		}
	}
	sortTasksNewestFirst(all)

	limit := queryInt(r, "limit", 50, 1, 200)
	offset := queryInt(r, "offset", 0, 0, 1<<20)
	total := len(all)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": all[offset:end], "total": total,
		"limit": limit, "offset": offset, "has_more": end < total,
		"scanned_sessions": len(metas),
	})
}

// summarizeSteps keeps the shape of a task's progress without its detail.
func summarizeSteps(steps []Step) []Step {
	out := make([]Step, 0, len(steps))
	for _, s := range steps {
		s.Tools, s.Artifacts, s.Progress = nil, nil, ""
		out = append(out, s)
	}
	return out
}

// handleTask returns one task with its steps, durations and artifacts.
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	sessionID, round := r.PathValue("session"), r.PathValue("round")
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks, err := s.tasksOf(r, svc, sessionID, s.kindResolver())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var task *Task
	for i := range tasks {
		if tasks[i].Round == round {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}

	// Durations come from the metrics table, which is where they are measured.
	// The frames deliberately carry none: ADK merges parallel tool responses
	// into one event reusing the first one's timestamp, so a frame-to-frame
	// delta is not a duration.
	if tr := s.engine().Metrics(); tr != nil {
		if rows, err := tr.ByInvocation(sessionID, round); err == nil {
			byCall := make(map[string]int, len(rows))
			for _, c := range rows {
				byCall[c.CallID] = c.DurationMS
			}
			for i := range task.Steps {
				for j := range task.Steps[i].Tools {
					t := &task.Steps[i].Tools[j]
					t.DurationMS = byCall[t.CallID]
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task":      task,
		"artifacts": s.artifactsOf(r, sessionID, round, task),
	})
}

// Artifact is one thing a task produced.
type Artifact struct {
	Label  string `json:"label"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // tool_result | report
	Step   string `json:"step,omitempty"`
	At     int64  `json:"at,omitempty"`
	Bytes  int    `json:"bytes"`
	Lines  int    `json:"lines,omitempty"`

	// Complete says the model received the whole thing. False means it was cut
	// or withheld — either way the rest is only in the store.
	Complete    bool   `json:"complete"`
	Retrievable bool   `json:"retrievable"`
	Expired     bool   `json:"expired"`
	Upstream    string `json:"upstream,omitempty"`
	Text        string `json:"text,omitempty"` // reports only; never a stored payload
}

// artifactsOf lists what a task produced, without reading any of it.
//
// The final answer is included as an artifact of kind "report" because it is
// one — it is the thing the run was for — but it is the only one carried
// inline, and only because it is already in the task.
func (s *Server) artifactsOf(r *http.Request, sessionID, round string, task *Task) []Artifact {
	stepOf := map[string]string{}
	for _, st := range task.Steps {
		for _, ref := range st.Artifacts {
			stepOf[ref] = st.ID
		}
	}
	completeOf, retrOf, linesOf := map[string]bool{}, map[string]bool{}, map[string]int{}
	for _, st := range task.Steps {
		for _, t := range st.Tools {
			if t.EvidenceID == "" {
				continue
			}
			completeOf[t.EvidenceID] = !t.Truncated && !t.Withheld
			retrOf[t.EvidenceID] = t.Retrievable
			linesOf[t.EvidenceID] = t.Lines
		}
	}

	out := []Artifact{}
	if store, err := s.engine().Records(); err == nil {
		items, err := store.List(r.Context(), record.Scope{
			AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID,
		}, record.ListOpts{InvocationID: round})
		if err == nil {
			for _, it := range items {
				out = append(out, Artifact{
					Label: it.Label, CallID: it.CallID, Name: it.Tool, Kind: "tool_result",
					Step: stepOf[it.Label], At: it.At.UnixMilli(), Bytes: it.Bytes,
					Lines: linesOf[it.Label], Complete: completeOf[it.Label],
					// Retrievable is what the store can answer now, not what the
					// model was told then: an expired row was retrievable when it
					// was cited and is not any more.
					Retrievable: !it.Expired && retrOf[it.Label],
					Expired:     it.Expired, Upstream: string(it.Upstream),
				})
			}
		}
	}
	if strings.TrimSpace(task.Answer) != "" {
		out = append(out, Artifact{
			Label: "report", Name: "最终报告", Kind: "report",
			At: task.EndedAt, Bytes: len(task.Answer), Complete: true,
			Text: task.Answer,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}
