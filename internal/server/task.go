package server

// A task is a goal the user set, and everything done to reach it.
//
// Not a session: a conversation holds several goals and a lot of chat that is
// not a goal at all. Not a message: "你好" is a message and nothing was done
// about it. Not a tool call: calling query_instant and then query_range is one
// piece of work, and showing them as two phases describes the plumbing rather
// than the work.
//
// So three separate judgements, and each is made from a fact rather than from
// wording:
//
//   which runs are tasks   — did anything happen (tools, records, a failure)
//   what the steps are     — what each tool call was FOR, not what it was
//   what the answer is     — the reply that had nothing following it
//
// The last one is the subtlest. A model that says "找到数据源了，现在拉近
// 24h 的曲线" and then calls a tool is narrating; a model that says something
// and calls nothing has finished. Both are top-level assistant text and they
// are indistinguishable after the fact — so the projector marks it at the
// point where the event still knows (see timeline.go, the "final" key).

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/task"
)

// Task statuses.
//
// The first five are produced. waiting_input and blocked are declared because
// the frontend renders them and a second, divergent list elsewhere is worse
// than an unused constant — but nothing produces them yet: no tool can say it
// is waiting for a person, because the approval machinery is not wired.
const (
	TaskPending      = "pending"
	TaskRunning      = "running"
	TaskCompleted    = "completed"
	TaskFailed       = "failed"
	TaskCancelled    = "cancelled"
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
// the identity there — see ensureSession — so it is a fact, not a guess about
// content.
const schedulePrefix = "schedule-"

// Phases are what a stretch of work was for.
//
// This is the grouping the page needs and the one the old version got wrong:
// it grouped by which tool was called, so two ways of asking the same
// monitoring system became two phases. What matters is the purpose, and the
// purpose is derivable — a tool that fetches from outside is collecting, a
// tool that reads back something already fetched is analysing, a tool that
// changes something is acting.
const (
	phaseCollect  = "collect"
	phaseAnalyze  = "analyze"
	phaseAct      = "act"
	phaseUnknown  = "unknown"
	phaseConclude = "conclude"
)

// readers are the delivery store's own tools: they operate on what an earlier
// step already fetched, which is what makes them analysis rather than
// collection however they are classified elsewhere.
var readers = map[string]bool{
	"read_result": true, "search_result": true, "stats_result": true,
}

// phaseLabels name a phase for someone who does not know the tools.
var phaseLabels = map[string]string{
	phaseCollect: "查询数据",
	phaseAnalyze: "分析数据",
	phaseAct:     "执行变更",
	phaseUnknown: "执行工具",
}

// collectLabels refine "查询数据" when the tool said what it produces.
//
// Only when it said so. Every MCP tool arrives with no declared kind, and
// naming its phase "定位异常" would be describing what the model meant by the
// call, which nothing recorded.
var collectLabels = map[ops.EvidenceKind]string{
	ops.KindMetricSeries:   "查询监控",
	ops.KindWorkloadStatus: "查询状态",
	ops.KindEvents:         "查询事件",
	ops.KindLogExcerpt:     "关联日志",
	ops.KindTableRows:      "查询数据",
	ops.KindConfig:         "读取配置",
	ops.KindTopology:       "梳理拓扑",
	ops.KindKnowledge:      "检索知识",
}

// ToolInfo is what the projector knows about a tool.
type ToolInfo struct {
	Kind     ops.EvidenceKind
	Mutating bool
	Known    bool
}

// phaseOf classifies one call.
func phaseOf(tool string, info ToolInfo, args map[string]any) string {
	if readers[tool] {
		return phaseAnalyze
	}
	// A call that takes an evidence handle is working on what an earlier step
	// produced, whatever it is named — the input reference is the evidence.
	if referencesEvidence(args) {
		return phaseAnalyze
	}
	if info.Mutating {
		return phaseAct
	}
	if info.Known {
		return phaseCollect
	}
	// An unknown tool still has to appear. Grouping it as collection would
	// claim to know what it did; "执行工具" claims only that it ran.
	return phaseUnknown
}

// referencesEvidence reports whether a call's arguments cite a stored result.
//
// The handles are "e" plus digits (record.Label). Matching the shape rather
// than a parameter name because the name differs per tool and the shape does
// not.
func referencesEvidence(args map[string]any) bool {
	for k, v := range args {
		s, ok := v.(string)
		if !ok || len(s) < 2 || s[0] != 'e' {
			continue
		}
		digits := true
		for _, r := range s[1:] {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits && (k == "ref" || k == "evidence_id" || strings.HasSuffix(k, "_ref")) {
			return true
		}
	}
	return false
}

// StepTool is one tool call inside a step.
type StepTool struct {
	Name string `json:"name"`
	// CallID identifies the call within its run, and only within its run: two
	// runs of one task both number their first call c1. Round is what makes it
	// unique, and every consumer that indexes by call must pair the two.
	CallID     string         `json:"call_id"`
	Round      string         `json:"round"`
	Args       map[string]any `json:"args,omitempty"` // redacted; see redactArgs
	OK         bool           `json:"ok"`
	Pending    bool           `json:"pending"`
	Error      string         `json:"error,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	EvidenceID string         `json:"evidence_id,omitempty"`
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

// Step is a stretch of work with one purpose.
type Step struct {
	ID        string     `json:"id"`
	Index     int        `json:"index"`
	Label     string     `json:"label"`
	Phase     string     `json:"phase"`
	Kind      string     `json:"kind,omitempty"`
	Status    string     `json:"status"`
	Agent     string     `json:"agent,omitempty"`
	StartedAt int64      `json:"started_at,omitempty"`
	EndedAt   int64      `json:"ended_at,omitempty"`
	Tools     []StepTool `json:"tools,omitempty"`
	Calls     int        `json:"calls"`
	Artifacts []string   `json:"artifacts,omitempty"`
	// Note is what the model said it was doing while this step ran — its own
	// narration, quoted, not a summary invented here.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// Task is a goal and everything done to reach it.
type Task struct {
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	Round     string   `json:"round"` // the run that opened it
	Runs      []string `json:"runs,omitempty"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	StartedAt int64    `json:"started_at,omitempty"`
	EndedAt   int64    `json:"ended_at,omitempty"`
	Steps     []Step   `json:"steps,omitempty"`

	// Reply is what the user actually received, in the model's own Markdown.
	// Never a tool result, never a progress line, never an invented summary.
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`

	// Chat marks a run that was only conversation. Kept rather than dropped
	// during the fold so the caller can say why a run is absent from the list.
	Chat bool `json:"chat,omitempty"`
}

// sensitiveArg matches parameters whose values must not be echoed to a screen.
//
// The console shows what a call was asked to do, and "what it was asked to do"
// legitimately includes a query. It does not include a credential, and a tool
// that takes one would otherwise print it into a page anyone with the console
// can read.
func sensitiveArg(name string) bool {
	n := strings.ToLower(name)
	for _, bad := range []string{"token", "password", "passwd", "secret", "key", "credential", "auth", "cookie"} {
		if strings.Contains(n, bad) {
			return true
		}
	}
	return false
}

// maxArgChars bounds one displayed argument. A PromQL expression is worth
// showing; a pasted log is not, and it would push everything else off screen.
const maxArgChars = 300

// redactArgs makes a call's arguments safe and small enough to display.
func redactArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if sensitiveArg(k) {
			out[k] = "＜已隐藏＞"
			continue
		}
		if s, ok := v.(string); ok {
			out[k] = truncateRunes(s, maxArgChars)
			continue
		}
		out[k] = v
	}
	return out
}

// foldTasks turns a session's frames into its tasks.
//
// linkOf maps an invocation to the task it was attached to; an absent entry
// means the run is its own task, which is the common case and the only case
// for anything recorded before tasks existed.
func foldTasks(sessionID string, frames []map[string]any, infoOf func(string) ToolInfo, linkOf map[string]string) []Task {
	type building struct {
		task   *Task
		steps  []*Step
		cur    *Step
		byCall map[string]*Step
		kinds  map[ops.EvidenceKind]int
		tools  int
		seen   map[string]bool // invocations folded into this task
		// errs holds the model failures reported per run, in the order they
		// happened, and answered says which runs went on to reply anyway.
		//
		// Kept apart and resolved at the end rather than applied on the spot,
		// because a failure is only final if the run never answered. Applied
		// on the spot, a model that was refused, retried and answered was
		// shown as completed live and as failed on every reload — the same run
		// telling two stories depending on which surface you looked at.
		errs     []roundError
		answered map[string]bool
	}
	order := []string{}
	byTask := map[string]*building{}

	taskFor := func(round string) *building {
		id := taskIDFor(sessionID, round, linkOf)
		b, ok := byTask[id]
		if !ok {
			s, inv := task.Split(id)
			b = &building{
				task: &Task{
					ID: id, SessionID: s, Round: inv,
					Status: TaskCompleted, Type: TypeOther,
				},
				byCall:   map[string]*Step{},
				kinds:    map[ops.EvidenceKind]int{},
				seen:     map[string]bool{},
				answered: map[string]bool{},
			}
			byTask[id] = b
			order = append(order, id)
		}
		if !b.seen[round] {
			b.seen[round] = true
			b.task.Runs = append(b.task.Runs, round)
		}
		return b
	}

	// The user message that opened a run titles the task that run opened. A
	// run joined to an existing task does not retitle it — the goal was set
	// once, and the follow-up is part of it.
	pendingTitle := ""

	for _, f := range frames {
		typ, _ := f["type"].(string)
		round, _ := f["round"].(string)
		ts := frameInt64(f, "ts")

		switch typ {
		case frameUserMessage:
			pendingTitle, _ = f["text"].(string)
			continue
		case frameError:
			// The stream-level error — the transport gave out, and nothing
			// after it is trustworthy — carries no round and fails what is
			// open. A model-reported failure names its run, and goes through
			// the ordinary path below so that it can open a task of its own:
			// a question that was refused before any tool ran produced no
			// other frame at all, so there was nothing for the failure to
			// attach to and the run vanished from the board entirely.
			if round == "" {
				msg, _ := f["message"].(string)
				for _, id := range order {
					failTask(byTask[id].task, msg)
				}
				continue
			}
		}
		switch typ {
		case frameToolCall, frameToolResult, frameText, frameThought, frameLLMTurn, frameError:
		default:
			continue
		}
		if round == "" {
			round = sessionID
		}
		b := taskFor(round)
		if b.task.StartedAt == 0 && ts > 0 {
			b.task.StartedAt = ts
		}
		if ts > b.task.EndedAt {
			b.task.EndedAt = ts
		}
		if b.task.Title == "" && pendingTitle != "" {
			b.task.Title = pendingTitle
		}
		pendingTitle = ""

		switch typ {
		case frameError:
			msg, _ := f["message"].(string)
			b.errs = append(b.errs, roundError{round: round, msg: msg})

		case frameToolCall:
			name, _ := f["name"].(string)
			args, _ := f["args"].(map[string]any)
			callID, _ := f["call_id"].(string)
			agent, _ := f["agent"].(string)
			info := infoOf(name)
			phase := phaseOf(name, info, args)
			label := stepLabel(phase, info.Kind)

			// Same purpose continues the step. Not the same tool: two ways of
			// asking one monitoring system are one piece of work, and that is
			// the case the old grouping split.
			//
			// Same agent, though. A step is attributed to one agent and shown
			// under its name, so letting a coordinator's call and a
			// specialist's merge would file one of them under the other's
			// name — and the handover, which is the most interesting thing
			// that happened in a delegated run, would leave no mark at all.
			same := b.cur != nil && b.cur.Phase == phase && b.cur.Label == label &&
				b.cur.Agent == agent
			if !same {
				b.cur = &Step{
					ID: "s" + strconv.Itoa(len(b.steps)+1), Index: len(b.steps),
					Phase: phase, Kind: string(info.Kind), Label: label,
					Status: TaskRunning, Agent: agent, StartedAt: ts,
				}
				b.steps = append(b.steps, b.cur)
			}
			b.cur.Calls++
			b.tools++
			b.cur.Tools = append(b.cur.Tools, StepTool{
				Name: name, CallID: callID, Round: round,
				Args: redactArgs(args), Pending: true,
			})
			if callID != "" {
				b.byCall[round+"/"+callID] = b.cur
			}
			if info.Kind != "" {
				b.kinds[info.Kind]++
			}

		case frameToolResult:
			callID, _ := f["call_id"].(string)
			step := b.byCall[round+"/"+callID]
			if step == nil {
				step = b.cur
			}
			if step == nil {
				continue
			}
			if ts > step.EndedAt {
				step.EndedAt = ts
			}
			ok, _ := f["ok"].(bool)
			errText, _ := f["error"].(string)
			resp, _ := f["response"].(map[string]any)

			for i := range step.Tools {
				// Both halves: a merged task holds several runs, and matching
				// on the call id alone attached a follow-up run's result to
				// the first run's call.
				if step.Tools[i].CallID != callID || step.Tools[i].Round != round {
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
				step.Status, step.Error = TaskFailed, firstNonEmpty(errText, step.Error)
			} else if step.Status == TaskRunning && allSettled(step) {
				step.Status = TaskCompleted
			}

		case frameText:
			text, _ := f["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			final, _ := f["final"].(bool)
			// Depth used to decide this: root-level text was the conclusion,
			// anything from a sub-agent was working notes. That holds right up
			// until a coordinator delegates and then says nothing more, which
			// is the normal shape of one — transfer_to_agent hands the turn
			// over and the specialist's reply IS what the user received. The
			// task then had no reply at all and no 形成结论 step, while the
			// answer sat in the timeline looking like a note.
			//
			// So the server's own final flag decides instead, at any depth.
			// Narration is not marked final, so the split it was protecting —
			// notes stay in their step, conclusions become one — is intact.
			if !final {
				// Narration: the model said what it was about to do and then
				// did it. It belongs to the step it introduced, and it must
				// not close that step — treating it as an answer is what put
				// query_instant and query_range in separate phases.
				if b.cur != nil && b.cur.Note == "" {
					b.cur.Note = firstLine(text)
				}
				continue
			}
			// A conclusion is a step, at the point it happened.
			//
			// Appending it once at the end would put a task's two replies —
			// "tell me the threshold", then the answer that used it — into one
			// box at the bottom, out of order relative to the work between
			// them. Steps are shown in the order they occurred, so this
			// belongs where it occurred.
			if b.cur != nil && b.cur.Status == TaskRunning {
				b.cur.Status = TaskCompleted
			}
			b.steps = append(b.steps, &Step{
				ID: "s" + strconv.Itoa(len(b.steps)+1), Index: len(b.steps),
				Label: "形成结论", Phase: phaseConclude, Status: TaskCompleted,
				StartedAt: ts, EndedAt: ts, Note: firstLine(text),
			})
			// The task's reply is the latest one, because that is what the
			// user last received.
			b.task.Reply = text
			b.answered[round] = true
			b.cur = nil
		}
	}

	out := make([]Task, 0, len(order))
	for _, id := range order {
		b := byTask[id]
		for _, s := range b.steps {
			if s.Status == TaskRunning && allSettled(s) {
				s.Status = TaskCompleted
			}
			b.task.Steps = append(b.task.Steps, *s)
		}
		// A model failure stands only if that run never answered. This is the
		// same rule the live stream applies, and it has to be the same rule:
		// otherwise a recovered turn reads as completed while it is streaming
		// and as failed the moment the page is reloaded.
		for _, e := range b.errs {
			if !b.answered[e.round] {
				failTask(b.task, e.msg)
			}
		}
		b.task.Type = taskType(b.task.SessionID, b.kinds)
		if b.task.Title == "" {
			b.task.Title = fallbackTitle(b.task)
		}
		for _, s := range b.task.Steps {
			if s.Status == TaskFailed {
				b.task.Status = TaskFailed
				break
			}
		}
		// Conversation, not work. Kept in the result so a caller can say why a
		// run is absent rather than silently losing it.
		b.task.Chat = b.tools == 0 && !strings.HasPrefix(b.task.SessionID, schedulePrefix)
		out = append(out, *b.task)
	}
	return out
}

// IsTask reports whether a run belongs in the task centre.
//
// Deterministic, and never about wording. Something happened if a tool ran, if
// the scheduler ran it, if it produced a stored result, or if it ended in a
// state a person has to do something about. "你好" satisfies none of those and
// is a conversation, which is a different thing the console already shows.
// hasRecords is a function, not a value, because answering it costs a query
// against the delivery store — and every other test here is a field
// comparison. Ordered so the cheap ones run first: it is consulted only for a
// run that looks like conversation by every other measure, which is a small
// fraction of them and the only case where the answer changes anything.
func IsTask(t Task, hasRecords func() bool) bool {
	if strings.HasPrefix(t.SessionID, schedulePrefix) {
		return true
	}
	switch t.Status {
	case TaskFailed, TaskWaitingInput, TaskBlocked:
		return true
	}
	if !t.Chat {
		return true
	}
	return hasRecords != nil && hasRecords()
}

// readResponse lifts what the gateway already told the model into the step.
// These keys come from gateway.toolPayload, so nothing here is inferred.
func readResponse(t *StepTool, resp map[string]any) {
	if resp == nil {
		return
	}
	t.EvidenceID, _ = resp["evidence_id"].(string)
	t.Summary, _ = resp["summary"].(string)
	t.Summary = truncateRunes(t.Summary, 300)
	t.Retrievable, _ = resp["retrievable"].(bool)
	t.Truncated, _ = resp["truncated"].(bool)
	if ov, ok := resp["overview"].(map[string]any); ok {
		t.Bytes = int(frameInt64(ov, "bytes"))
		t.Lines = int(frameInt64(ov, "lines"))
		if note, _ := ov["note"].(string); note != "" {
			t.Withheld = true
		}
	}
}

func stepLabel(phase string, kind ops.EvidenceKind) string {
	if phase == phaseCollect {
		if l, ok := collectLabels[kind]; ok && kind != "" {
			return l
		}
	}
	if l, ok := phaseLabels[phase]; ok {
		return l
	}
	return phaseLabels[phaseUnknown]
}

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

func fallbackTitle(t *Task) string {
	for _, s := range t.Steps {
		if s.Phase != phaseConclude {
			return s.Label
		}
	}
	return "（无标题）"
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
	if i := strings.IndexAny(s, "\n"); i > 0 {
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

// frameInt64 reads a number out of a frame payload. Frames arrive as Go values
// on the replay path and as JSON on the wire, so a count can be an int, an
// int32 or a float64 depending on which side built it.
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

func sortTasksNewestFirst(ts []Task) {
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].StartedAt > ts[j].StartedAt })
}

// taskIDFor is which task a run belongs to: the one it was joined to, or its
// own. One definition, because a second one drifting from this is how a run
// ends up folded into a task in one view and standing alone in another.
func taskIDFor(sessionID, round string, linkOf map[string]string) string {
	if id := linkOf[round]; id != "" {
		return id
	}
	return task.ID(sessionID, round)
}

// failTask records a failure without displacing an earlier one. What stopped a
// run explains the rest, so the first message is the one worth keeping.
func failTask(t *Task, msg string) {
	if t.Status == TaskRunning || t.Status == TaskCompleted {
		t.Status = TaskFailed
	}
	if msg != "" && t.Error == "" {
		t.Error = msg
	}
}

// roundError is one run's model-reported failure, kept until the fold knows
// whether that run went on to answer anyway.
type roundError struct{ round, msg string }
