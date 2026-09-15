package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"google.golang.org/adk/agent"
	"google.golang.org/genai"
)

// annotateTimeout bounds one label-generation run. It reads across a whole
// repository, so it is generous — but not unbounded, because every turn costs
// tokens and a run nobody is watching should still end.
const annotateTimeout = 30 * time.Minute

// annotatePrompt drives the run. It is written here rather than left to the
// user because the cheap path through a large monorepo is not obvious: one
// grep over the contract files identifies most services in a single call,
// where opening 130 directories one at a time would cost a hundred times more.
func annotatePrompt(p codeproject.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "给代码项目 %q（标识 %s）的目录做业务标注。\n\n", p.Name, p.ID)
	b.WriteString("步骤：\n")
	fmt.Fprintf(&b, "1. list_project_dir 看清 %s 下的目录结构，确定要标注哪些目录。\n", p.Main())
	b.WriteString("2. 先用少量 grep_project_files 批量获取线索，不要逐个目录翻文件。对 Go 微服务仓库，")
	b.WriteString("grep pattern \"^service \"、glob \"*.proto\"、max_matches 500 通常一次就能认出大多数服务；")
	b.WriteString("README、入口 main.go、包注释也可以用同样的方式批量取。\n")
	b.WriteString("3. 只有在批量线索不足以判断某个目录时，才单独读它的文件。\n")
	b.WriteString("4. 用 propose_project_dir_info 分批提交草稿，每批不超过 30 个目录：\n")
	b.WriteString("   - name：简短的中文业务名称\n")
	b.WriteString("   - description：这个目录负责什么、边界在哪，一到两句\n")
	b.WriteString("   - tags：2–3 个分类标签（如 支付 / 营销 / 内容 / 后台 / 定时任务 / 核心链路）\n\n")
	b.WriteString("要求：只写有依据的结论，依据来自你实际读到的内容；判断不了的目录直接跳过，")
	b.WriteString("不要用目录名硬猜。提交的是草稿，用户会在页面上审核，所以宁可少标也不要标错。\n")
	b.WriteString("全部提交完后，用一两句话说明你标注了多少个目录、跳过了哪些、依据是什么。")
	return b.String()
}

// handleAnnotateCodeProject starts a label-generation run in the background.
//
// It runs as the agent the project is assigned to, so propose_project_dir_info
// sees exactly the grant an ordinary analysis would — the button is a shortcut
// for typing the prompt, not a way around the assignment.
func (s *Server) handleAnnotateCodeProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	store := s.engineFor(r).CodeProjects()
	if store.AnnotateRunning(id) {
		writeErr(w, http.StatusConflict, "已有一个标注生成任务在运行，请等它结束")
		return
	}
	agentName, err := store.AssignedAgent(id)
	if err != nil {
		projectError(w, err)
		return
	}
	var project *codeproject.Project
	ps, err := store.List()
	if err != nil {
		writeErr(w, 500, "无法读取项目配置")
		return
	}
	for i := range ps {
		if ps[i].ID == id {
			project = &ps[i]
		}
	}
	if project == nil {
		projectError(w, codeproject.ErrNotFound)
		return
	}
	if project.SyncedAt == nil {
		writeErr(w, 400, "项目尚未同步，请先同步代码再生成标注")
		return
	}

	now := time.Now().UTC()
	run := &codeproject.AnnotateRun{
		State: "running", TaskID: newAnnotateTaskID(), Agent: agentName,
		Session: "annotate-" + id, StartedAt: &now,
	}
	if err := store.SetAnnotateRun(id, run); err != nil {
		projectError(w, err)
		return
	}
	eng := s.engineFor(r)
	before := store.PendingDrafts(id)
	go s.runAnnotate(eng, store, *project, agentName, *run, before)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task_id": run.TaskID, "agent": agentName, "session": run.Session})
}

// runAnnotate executes the run detached from the request, like a sync: it
// outlives the browser tab, and the page follows it through the project state.
func (s *Server) runAnnotate(eng *engine.Engine, store *codeproject.Store, p codeproject.Project, agentName string, run codeproject.AnnotateRun, before int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), annotateTimeout)
	defer cancel()

	err := s.driveAnnotateAgent(ctx, eng, agentName, run.Session, annotatePrompt(p))
	done := run
	done.Proposed = store.PendingDrafts(p.ID) - before
	if done.Proposed < 0 {
		done.Proposed = 0
	}
	if err != nil {
		done.State, done.Error = "failed", err.Error()
		slog.Warn("代码标注生成失败", "project", p.ID, "agent", agentName, "err", err)
	} else {
		done.State, done.Error = "succeeded", ""
	}
	if err := store.SetAnnotateRun(p.ID, &done); err != nil {
		slog.Warn("标注任务状态写入失败", "project", p.ID, "err", err)
	}
}

func (s *Server) driveAnnotateAgent(ctx context.Context, eng *engine.Engine, agentName, session, prompt string) error {
	a, _, _, search, err := eng.BuildAgentByName(agentName)
	if err != nil {
		return err
	}
	if search != nil {
		defer search.Close()
	}
	r, svc, err := eng.NewRunner(a, search)
	if err != nil {
		return err
	}
	if err := ensureSession(ctx, svc, session); err != nil {
		return err
	}
	for _, err := range r.Run(ctx, engine.UserID, session,
		genai.NewContentFromText(prompt, genai.RoleUser),
		agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return err
		}
	}
	return nil
}

func newAnnotateTaskID() string {
	return fmt.Sprintf("annotate-%d", time.Now().UnixNano())
}
