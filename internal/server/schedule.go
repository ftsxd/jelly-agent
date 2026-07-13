package server

import (
	"context"
	"fmt"
	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	"github.com/jelly-agent/jelly-agent/internal/schedule"
	"github.com/robfig/cron/v3"
	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
	"net/http"
	"strings"
	"sync"
	"time"
)

type scheduler struct {
	mu     sync.Mutex
	c      *cron.Cron
	cancel context.CancelFunc
}

type createScheduleArgs struct {
	Name   string `json:"name"`
	Cron   string `json:"cron"`
	Prompt string `json:"prompt"`
	Skill  string `json:"skill,omitempty"`
}

func (s *Server) attachScheduleTools(eng *engine.Engine) {
	t, err := functiontool.New(functiontool.Config{Name: "create_schedule", Description: "创建标准 Cron 周期任务。当用户要求定时、每天、每周执行任务时调用。"}, func(_ adktool.Context, a createScheduleArgs) (map[string]any, error) {
		task := config.ScheduleTask{Name: strings.TrimSpace(a.Name), Cron: strings.TrimSpace(a.Cron), Prompt: strings.TrimSpace(a.Prompt), Skill: strings.TrimSpace(a.Skill), Enabled: true}
		if err := validSchedule(task); err != nil {
			return nil, err
		}
		p, err := s.writeTargetPath()
		if err != nil {
			return nil, err
		}
		c, err := loadRawOrEmpty(p)
		if err != nil {
			return nil, err
		}
		c.Schedules = append(c.Schedules, task)
		if err := config.Save(c, p); err != nil {
			return nil, err
		}
		if err := s.reload(); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "name": task.Name}, nil
	})
	if err == nil {
		eng.SetExtraTools([]adktool.Tool{t})
	}
}

func (s *Server) StartSchedules(ctx context.Context) {
	s.stopSchedules()
	c := cron.New()
	runCtx, cancel := context.WithCancel(ctx)
	for _, t := range s.engine().Config().Schedules {
		if !t.Enabled {
			continue
		}
		task := t
		if _, err := c.AddFunc(task.Cron, func() { s.runSchedule(runCtx, task) }); err != nil {
			s.logf("周期任务 %q Cron 无效: %v", task.Name, err)
		}
	}
	s.schedule.mu.Lock()
	s.schedule.c = c
	s.schedule.cancel = cancel
	s.schedule.mu.Unlock()
	c.Start()
}
func (s *Server) stopSchedules() {
	s.schedule.mu.Lock()
	defer s.schedule.mu.Unlock()
	if s.schedule.cancel != nil {
		s.schedule.cancel()
	}
	if s.schedule.c != nil {
		ctx := s.schedule.c.Stop()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	s.schedule.c = nil
	s.schedule.cancel = nil
}
func (s *Server) runSchedule(ctx context.Context, t config.ScheduleTask) {
	started := time.Now()
	prompt := t.Prompt
	if t.Skill != "" {
		prompt = "必须先调用 use_skill 加载技能 \"" + t.Skill + "\"，再严格执行。\n\n" + prompt
	}
	out, err := s.runScheduledAgent(ctx, t, prompt)
	if err != nil {
		_ = schedule.Record(t.Name, started, "failed", out, err.Error())
		s.logf("周期任务 %q 失败: %v", t.Name, err)
		return
	}
	_ = schedule.Record(t.Name, started, "succeeded", out, "")
}
func (s *Server) runScheduledAgent(ctx context.Context, t config.ScheduleTask, prompt string) (string, error) {
	eng := s.engine()
	name := t.Agent
	if name == "" && eng.HasAgents() {
		name = eng.DefaultAgentName()
	}
	var a agent.Agent
	var search *memory.Search
	var err error
	if name != "" {
		a, _, _, search, err = eng.BuildAgentByName(name)
	} else {
		a, _, _, search, err = eng.BuildAgent(t.Provider)
	}
	if err != nil {
		return "", err
	}
	if search != nil {
		defer search.Close()
	}
	r, svc, err := eng.NewRunner(a, search)
	if err != nil {
		return "", err
	}
	id := "schedule-" + t.Name
	if _, err := s.resolveSession(ctx, svc, id); err != nil {
		return "", err
	}
	var b strings.Builder
	for ev, err := range r.Run(ctx, engine.UserID, id, genai.NewContentFromText(prompt, genai.RoleUser), agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return b.String(), err
		}
		if ev != nil && ev.Content != nil && ev.Partial {
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought {
					b.WriteString(p.Text)
				}
			}
		}
	}
	return b.String(), nil
}
func validSchedule(t config.ScheduleTask) error {
	if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("name 和 prompt 不能为空")
	}
	if _, err := cron.ParseStandard(t.Cron); err != nil {
		return fmt.Errorf("Cron 无效: %w", err)
	}
	return nil
}

func (s *Server) handleSchedules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"schedules": s.engine().Config().Schedules})
}
func (s *Server) handleSaveSchedule(w http.ResponseWriter, r *http.Request) {
	var t config.ScheduleTask
	if err := decodeJSON(r, &t); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t.Name = strings.TrimSpace(t.Name)
	t.Cron = strings.TrimSpace(t.Cron)
	if err := validSchedule(t); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	c, err := loadRawOrEmpty(p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	found := false
	for i := range c.Schedules {
		if c.Schedules[i].Name == t.Name {
			c.Schedules[i] = t
			found = true
		}
	}
	if !found {
		c.Schedules = append(c.Schedules, t)
	}
	if err := s.persist(w, c, p); err != nil {
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	c, err := loadRawOrEmpty(p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := c.Schedules[:0]
	for _, t := range c.Schedules {
		if t.Name != name {
			out = append(out, t)
		}
	}
	c.Schedules = out
	if err := s.persist(w, c, p); err != nil {
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleScheduleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := schedule.List(r.URL.Query().Get("task"), 50)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
}
