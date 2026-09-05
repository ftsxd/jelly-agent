package server

// A run, seen as a task rather than as a conversation.
//
// Everything a task centre needs already happened and was already recorded —
// it just was not shaped this way. The event stream carries the tool calls and
// their results, tool_calls carries the durations, tool_results carries the
// payloads. What was missing was an identity for "one piece of work" and a
// projection that groups the calls into steps.
//
// The identity was already there too: a task is one invocation. Every table
// involved is keyed by (session_id, invocation_id, call_id), so a task id is
// just the first two joined — no new table, no new key, and nothing that can
// disagree with the events, because the events are what it is derived from.
//
// One projection, used by both the list and the detail. A second one for the
// list "because it only needs a summary" is how the two would start disagreeing
// about how many steps a task had.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// Task statuses. Four are produced today; the rest are declared because the
// frontend renders them and because leaving them out would invite a second,
// divergent list somewhere else.
//
// pending, waiting_input and blocked have no source yet: nothing in the system
// can say a step is waiting for a person, because the approval machinery is
// not wired (see engine.go, AllowApprovalRequired). They are not produced, and
// nothing pretends otherwise.
const (
	TaskRunning   = "running"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"

	TaskPending      = "pending"
	TaskWaitingInput = "waiting_input"
	TaskBlocked      = "blocked"
)

// Task types.
const (
	TypeMonitor    = "monitor"
	TypeLog        = "log"
	TypeInspection = "inspection"
	TypeOther      = "other"
)

// schedulePrefix marks the sessions the cron scheduler runs in. The name is
// the identity there — see ensureSession — so it is a reliable signal, not a
// guess about content.
const schedulePrefix = "schedule-"

// StepTool is one tool call inside a step.
type StepTool struct {
	Name       string `json:"name"`
	CallID     string `json:"call_id"`
	OK         bool   `json:"ok"`
	Pending    bool   `json:"pending"`
	Error      string `json:"error,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	// DurationMS comes from the tool_calls table, not from the frames: ADK
	// merges parallel responses into one event reusing the first one's
	// timestamp, so a frame-to-frame delta is not a duration.
	DurationMS  int  `json:"duration_ms,omitempty"`
	Retrievable bool `json:"retrievable"`
	Truncated   bool `json:"truncated"`
	Withheld    bool `json:"withheld"`
	Bytes       int  `json:"bytes,omitempty"`
	Lines       int  `json:"lines,omitempty"`
}

// Step is a contiguous stretch of one kind of work.
type Step struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Label string `json:"label"`
	// Kind is the evidence kind the tools in this step produce, when they
	// declare one. Empty for tools that do not — every MCP tool today, since
	// adopted metadata carries no Produces.
	Kind      string     `json:"kind,omitempty"`
	Status    string     `json:"status"`
	Agent     string     `json:"agent,omitempty"`
	StartedAt int64      `json:"started_at,omitempty"`
	EndedAt   int64      `json:"ended_at,omitempty"`
	Tools     []StepTool `json:"tools,omitempty"`
	Calls     int        `json:"calls"`
	Artifacts []string   `json:"artifacts,omitempty"`
	Progress  string     `json:"progress,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Task is one piece of work: one invocation of the agent.
type Task struct {
	ID        string `json:"id"` // session + "/" + round
	SessionID string `json:"session_id"`
	Round     string `json:"round"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	StartedAt int64  `json:"started_at,omitempty"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	Steps     []Step `json:"steps,omitempty"`
	Answer    string `json:"answer,omitempty"`
	Error     string `json:"error,omitempty"`

	Usage map[string]any `json:"usage,omitempty"`
}

// TaskID joins the two ids every table is already keyed by.
func TaskID(session, round string) string { return session + "/" + round }

// kindLabels name what a step was doing, when the tools said what they produce.
//
// Only these are used — a step whose tools declare nothing is labelled by the
// tool instead. Inventing a phase name for it ("定位异常") would be describing
// the model's intent, which nothing recorded.
var kindLabels = map[ops.EvidenceKind]string{
	ops.KindMetricSeries:   "查询指标",
	ops.KindWorkloadStatus: "查询状态",
	ops.KindEvents:         "查询事件",
	ops.KindLogExcerpt:     "关联日志",
	ops.KindTableRows:      "查询数据",
	ops.KindConfig:         "读取配置",
	ops.KindTopology:       "梳理拓扑",
	ops.KindKnowledge:      "检索知识",
	ops.KindText:           "调用工具",
}

// foldTasks turns a session's frames into its tasks.
//
// kindOf resolves a tool name to what it produces; it may return "" for a tool
// that declares nothing, which is the normal case for MCP tools.
func foldTasks(sessionID string, frames []map[string]any, kindOf func(tool string) ops.EvidenceKind) []Task {
	type building struct {
		task   *Task
		steps  []*Step
		cur    *Step
		byCall map[string]*Step
		kinds  map[ops.EvidenceKind]int
	}
	order := []string{}
	byRound := map[string]*building{}

	get := func(round string) *building {
		if b, ok := byRound[round]; ok {
			return b
		}
		b := &building{
			task: &Task{
				ID: TaskID(sessionID, round), SessionID: sessionID, Round: round,
				Status: TaskCompleted, Type: TypeOther,
			},
			byCall: map[string]*Step{},
			kinds:  map[ops.EvidenceKind]int{},
		}
		byRound[round] = b
		order = append(order, round)
		return b
	}

	// The user message that opened the session titles its first task. Later
	// tasks in the same session take the text of the message that started
	// them, which the frames do not carry per round — so the fallback is the
	// tool work itself rather than a borrowed title from another question.
	var pendingTitle string

	for _, f := range frames {
		typ, _ := f["type"].(string)
		round, _ := f["round"].(string)
		ts := frameInt64(f, "ts")

		switch typ {
		case frameUserMessage:
			pendingTitle, _ = f["text"].(string)
			continue
		case frameError:
			// Not attached to a round: the error frame carries none, because
			// the stream may have failed before any invocation existed.
			for _, r := range order {
				b := byRound[r]
				if b.task.Status == TaskRunning || b.task.Status == TaskCompleted {
					b.task.Status = TaskFailed
					if msg, _ := f["message"].(string); msg != "" && b.task.Error == "" {
						b.task.Error = msg
					}
				}
			}
			continue
		}
		// Only the frames that describe work reach the fold. The others —
		// usage, agent_transfer, the deltas — either duplicate what a work
		// frame already said or carry no round at all, and a round-less frame
		// falling through to the fallback below invented an empty task out of
		// the per-turn usage line.
		switch typ {
		case frameToolCall, frameToolResult, frameText, frameThought, frameLLMTurn:
		default:
			continue
		}
		if round == "" {
			// ADK stamps an invocation id on every event, so this is only
			// reached by data that predates it or was written by hand. The
			// session then is one task: nothing told us where its runs
			// divided, and saying "one" is truer than dropping the work.
			round = sessionID
		}
		b := get(round)
		if b.task.StartedAt == 0 && ts > 0 {
			b.task.StartedAt = ts
		}
		if ts > b.task.EndedAt {
			b.task.EndedAt = ts
		}
		if b.task.Title == "" && pendingTitle != "" {
			b.task.Title, pendingTitle = pendingTitle, ""
		}

		switch typ {
		case frameToolCall:
			name, _ := f["name"].(string)
			callID, _ := f["call_id"].(string)
			agent, _ := f["agent"].(string)
			kind := kindOf(name)

			// A step is a contiguous stretch of the same kind of work. When
			// the tools declare no kind — every MCP tool today — the tool
			// itself is the grouping, so "three calls to get_logs" is one step
			// and switching tools starts another.
			label := stepLabel(kind, name)
			// Grouping follows the label, not the kind alone: the store's own
			// readers share a label and must share a step, or "searched then
			// read the same result" shows as two unrelated phases.
			same := b.cur != nil && b.cur.Status != "" && b.cur.Label == label &&
				(kind == "" || b.cur.Kind == string(kind))
			if !same {
				b.cur = &Step{
					ID: "t" + strconv.Itoa(len(b.steps)+1), Index: len(b.steps),
					Kind: string(kind), Status: TaskRunning, Agent: agent, StartedAt: ts,
					Label: label,
				}
				b.steps = append(b.steps, b.cur)
			}
			b.cur.Calls++
			b.cur.Tools = append(b.cur.Tools, StepTool{Name: name, CallID: callID, Pending: true})
			if callID != "" {
				b.byCall[callID] = b.cur
			}
			b.kinds[kind]++

		case frameToolResult:
			callID, _ := f["call_id"].(string)
			step := b.byCall[callID]
			if step == nil {
				step = b.cur
			}
			if step == nil {
				continue
			}
			step.EndedAt = ts
			ok, _ := f["ok"].(bool)
			errText, _ := f["error"].(string)
			resp, _ := f["response"].(map[string]any)

			for i := range step.Tools {
				if step.Tools[i].CallID != callID {
					continue
				}
				t := &step.Tools[i]
				t.Pending, t.OK, t.Error = false, ok, errText
				readResponse(t, resp)
				if t.EvidenceID != "" {
					step.Artifacts = appendUnique(step.Artifacts, t.EvidenceID)
				}
				break
			}
			if !ok {
				// The server judged this, not the browser: toolFailed reads
				// the payload the same way the metrics table does.
				step.Status, step.Error = TaskFailed, firstNonEmpty(errText, step.Error)
			} else if step.Status == TaskRunning && allSettled(step) {
				step.Status = TaskCompleted
			}
			if s, _ := resp["summary"].(string); s != "" {
				step.Progress = s
			}

		case frameText:
			// Only the top-level answer closes the task. A sub-agent's prose
			// is part of the work, not the conclusion.
			if branch, _ := f["branch"].(string); branch != "" {
				continue
			}
			text, _ := f["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			b.task.Answer = text
			if b.cur != nil && b.cur.Status == TaskRunning {
				b.cur.Status = TaskCompleted
			}
			b.cur = nil
		}
	}

	out := make([]Task, 0, len(order))
	for _, r := range order {
		b := byRound[r]
		if b.task.Answer != "" {
			b.steps = append(b.steps, &Step{
				ID: "t" + strconv.Itoa(len(b.steps)+1), Index: len(b.steps),
				Label: "形成结论", Status: TaskCompleted,
				StartedAt: b.task.EndedAt, EndedAt: b.task.EndedAt,
				Progress: firstLine(b.task.Answer),
			})
		}
		for _, s := range b.steps {
			if s.Status == TaskRunning && allSettled(s) {
				s.Status = TaskCompleted
			}
			b.task.Steps = append(b.task.Steps, *s)
		}
		b.task.Type = taskType(sessionID, b.kinds)
		if b.task.Title == "" {
			b.task.Title = fallbackTitle(b.task)
		}
		for _, s := range b.task.Steps {
			if s.Status == TaskFailed {
				b.task.Status = TaskFailed
				break
			}
		}
		out = append(out, *b.task)
	}
	return out
}

// readResponse lifts what the gateway already told the model into the step.
//
// These keys come from gateway.toolPayload, so nothing here is inferred: the
// browser is shown exactly what the model was shown.
func readResponse(t *StepTool, resp map[string]any) {
	if resp == nil {
		return
	}
	t.EvidenceID, _ = resp["evidence_id"].(string)
	t.Retrievable, _ = resp["retrievable"].(bool)
	t.Truncated, _ = resp["truncated"].(bool)
	if ov, ok := resp["overview"].(map[string]any); ok {
		t.Bytes = int(frameInt64(ov, "bytes"))
		t.Lines = int(frameInt64(ov, "lines"))
		// A note on the overview is how a withheld payload announces itself:
		// the round had no budget left, so only the scale and a preview went
		// into the prompt. Distinct from truncation, which is this tool's own
		// ceiling being hit.
		if note, _ := ov["note"].(string); note != "" {
			t.Withheld = true
		}
	}
}

// readers are the delivery store's own tools. They fetch what an earlier step
// already produced rather than producing anything, and both declare KindText —
// so without this they label their step "调用工具", which says nothing about
// the one thing that is actually distinctive about it.
//
// Still derived, not invented: it is a fact about which tools ran, not a guess
// at what the model meant by running them.
var readers = map[string]bool{"read_result": true, "search_result": true}

func stepLabel(kind ops.EvidenceKind, tool string) string {
	if readers[tool] {
		return "读取已存结果"
	}
	if l, ok := kindLabels[kind]; ok && kind != "" {
		return l
	}
	return tool
}

// taskType is derived from where the run happened and what it looked at.
func taskType(sessionID string, kinds map[ops.EvidenceKind]int) string {
	if strings.HasPrefix(sessionID, schedulePrefix) {
		return TypeInspection
	}
	best, n := ops.EvidenceKind(""), 0
	for k, c := range kinds {
		if k != "" && c > n {
			best, n = k, c
		}
	}
	switch best {
	case ops.KindMetricSeries, ops.KindWorkloadStatus, ops.KindEvents:
		return TypeMonitor
	case ops.KindLogExcerpt:
		return TypeLog
	default:
		return TypeOther
	}
}

// fallbackTitle names a task the frames gave no user message for.
func fallbackTitle(t *Task) string {
	if len(t.Steps) > 0 {
		names := make([]string, 0, 3)
		for _, s := range t.Steps {
			if len(names) == 3 {
				break
			}
			names = append(names, s.Label)
		}
		return strings.Join(names, " → ")
	}
	return "（无内容）"
}

func allSettled(s *Step) bool {
	for _, t := range s.Tools {
		if t.Pending {
			return false
		}
	}
	return len(s.Tools) > 0
}

func appendUnique(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n。"); i > 0 {
		s = s[:i]
	}
	return truncateRunes(s, 120)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// frameInt64 reads a number out of a frame payload.
//
// Frames arrive as Go values on the replay path and as JSON on the wire, so a
// count can be an int, an int32 or a float64 depending on which side built it.
func frameInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// sortTasksNewestFirst is what a list wants; foldTasks returns them in the
// order they ran, which is what a session view wants.
func sortTasksNewestFirst(ts []Task) {
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].StartedAt > ts[j].StartedAt })
}
