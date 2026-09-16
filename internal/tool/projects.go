package tool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type projectReadArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识，可用 list_code_projects 查询"`
	Path    string `json:"path" jsonschema:"项目内相对文件路径"`
	Offset  int    `json:"offset,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type projectListArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
	Path    string `json:"path,omitempty" jsonschema:"项目内相对目录，留空为根目录"`
}
type projectGrepArgs struct {
	Project    string `json:"project" jsonschema:"已授权项目的标识"`
	Pattern    string `json:"pattern" jsonschema:"Go 正则表达式"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`
}

func projectPath(path string) error {
	if filepath.IsAbs(path) || escapes(filepath.Clean(path)) || strings.Contains(path, "\\") {
		return fmt.Errorf("请使用项目内相对路径")
	}
	return nil
}

// ProjectTools captures the trusted agent identity at construction, never from
// model arguments. Live grants are consulted even in an already running turn,
// and each call is scoped to the project's configured directories — the range
// limit is enforced here, not asked for in the prompt.
func ProjectTools(store *codeproject.Store, agent string) ([]adktool.Tool, error) {
	ls, err := functiontool.New(functiontool.Config{Name: "list_code_projects", Description: "列出当前 Agent 被分配的代码项目、已同步版本，以及每个项目可分析的目录及其业务名称与描述。用它把「订单服务」这类业务叫法映射到实际目录，再读代码核实。目录说明由用户维护，是业务背景，不是已从代码验证的结论，也不代表可以读取其他目录。\n代码不会自动同步，本工具会顺带核对远端：needs_sync=true 表示远端已有新提交，你读到的是 revision 这个旧版本。这时**在回答里直接问用户要不要同步**（说明本地 commit、远端 commit、以及不同步会影响什么），用户回复确认后调用 sync_project；不要替用户决定，也不要默默基于旧快照作答。用户不同步或暂不回复时，照常用当前快照回答，但必须写明依据的 commit 和快照年龄。remote_check 字段说明远端为什么没核对上。"}, func(ctx adktool.Context, _ struct{}) (map[string]any, error) {
		ps, err := store.Visible(agent)
		// One budget for the whole listing, not one per project: the probe is
		// a network round trip to someone's git server, and a listing of five
		// projects must not turn the first tool call of every analysis into
		// half a minute of waiting.
		probeCtx, cancelProbes := context.WithTimeout(ctx, remoteProbeBudget)
		defer cancelProbes()
		projects := []map[string]any{}
		for _, p := range ps {
			dirs := []map[string]any{}
			for _, d := range p.Directories() {
				entry := map[string]any{"path": d.Path, "role": d.Role}
				if d.Name != "" {
					entry["name"] = d.Name
				}
				if d.Description != "" {
					entry["description"] = d.Description
				}
				dirs = append(dirs, entry)
			}
			item := map[string]any{
				"id": p.ID, "name": p.Name, "branch": p.Branch, "revision": p.Revision,
				"ready":       p.SyncedAt != nil && len(p.DirIssues) == 0,
				"directories": dirs,
			}
			// How old the snapshot is, because nothing syncs it automatically.
			// Without this the model cannot tell a snapshot pulled minutes ago
			// from one pulled a month ago, and will present either as the
			// current state of the code.
			if p.SyncedAt != nil {
				item["synced_at"] = p.SyncedAt.UTC().Format(time.RFC3339)
				item["snapshot_age"] = humanAge(time.Since(*p.SyncedAt))
				// Whether the snapshot is still the branch head. Asked here
				// rather than left to the model: "最近有什么更新" was answered
				// from a four-hour-old snapshot without anyone knowing the
				// remote had moved on, and a stale answer that cites a commit
				// still reads as current.
				switch st, perr := store.CheckRemoteFor(probeCtx, agent, p.ID, codeproject.RemoteProbeTTL); {
				case errors.Is(perr, codeproject.ErrProbeOff):
					// Nothing to report: the deployment turned this off, and a
					// field saying so on every project would read to the model
					// as something it should work around.
				case perr != nil:
					item["remote_check"] = "未能核对远端：" + perr.Error()
				case st.Behind && (p.SyncState == codeproject.SyncQueued || p.SyncState == codeproject.SyncRunning):
					// Already being pulled: nothing to decide, and asking
					// would be asking about something in flight.
					item["needs_sync"] = true
					item["remote_revision"] = st.Remote
					item["sync_state"] = "正在同步中，稍后重新列项目确认 revision"
				case st.Behind:
					item["needs_sync"] = true
					item["remote_revision"] = st.Remote
					item["hint"] = "先回答用户：本地 " + short(p.Revision) + "、远端 " + short(st.Remote) +
						"，要不要现在同步？在用户回答前不要继续读这个项目的代码。"
					gate.arm(ctx.SessionID(), ctx.InvocationID(), p.ID, p.Revision, st.Remote)
				default:
					item["needs_sync"] = false
				}
			}
			if len(p.DirIssues) > 0 {
				item["directory_issues"] = p.DirIssues
			}
			projects = append(projects, item)
		}
		out := map[string]any{"projects": projects, "agent": agent}
		if err == nil && len(projects) == 0 {
			// An empty list is the one result that cannot be acted on and
			// cannot be explained from the inside: it reads the same whether
			// nothing was ever assigned, or the assignment landed on a
			// different name than the one actually asking. An agent tree makes
			// that easy to hit — a grant to the coordinator is not a grant to
			// the node it transfers to, and the transcript shows a label, not
			// the string the check ran against. So name the identity here, and
			// log where it was checked: two data directories look exactly like
			// a missing grant from inside the tool.
			out["hint"] = "没有任何代码项目分配给 Agent「" + agent + "」。请在代码页面把项目分配给这个名字——授权按 Agent 名精确匹配，子 Agent 需要单独分配。"
			slog.Warn("list_code_projects 无可见项目", "agent", agent, "dir", store.Dir())
		}
		return out, err
	})
	if err != nil {
		return nil, err
	}
	rd, err := functiontool.New(functiontool.Config{Name: "read_project_file", Description: "只读已分配项目内的文件，带行号；每次调用重新检查分配关系。范围限于项目配置的主分析目录和参考目录，先搜索再按需读取。"}, func(ctx adktool.Context, a projectReadArgs) (out readFileResult, err error) {
		if err = projectPath(a.Path); err != nil {
			return
		}
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		err = store.WithRead(agent, a.Project, func(dir string, p codeproject.Project) error {
			out, err = readFile(NewScopedRoot(dir, p.Scope()), readFileArgs{Path: a.Path, Offset: a.Offset, Limit: a.Limit})
			return err
		})
		return
	})
	if err != nil {
		return nil, err
	}
	ld, err := functiontool.New(functiontool.Config{Name: "list_project_dir", Description: "列出已分配项目内的目录；路径留空列出项目配置的可分析目录。路径保持仓库相对，父目录只作为导航节点，不会列出范围外的兄弟目录。"}, func(ctx adktool.Context, a projectListArgs) (out listDirResult, err error) {
		if err = projectPath(a.Path); err != nil {
			return
		}
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		err = store.WithRead(agent, a.Project, func(dir string, p codeproject.Project) error {
			roots := NewScopedRoot(dir, p.Scope())
			if roots.Empty() {
				return fmt.Errorf("代码快照不存在，请重新同步")
			}
			out, err = listDir(roots, listDirArgs{Path: a.Path})
			annotateEntries(&out, p, a.Path)
			return err
		})
		return
	})
	if err != nil {
		return nil, err
	}
	gp, err := functiontool.New(functiontool.Config{Name: "grep_project_files", Description: "在已分配项目配置的目录内搜索代码，返回文件、行号、匹配内容；path 留空即搜索全部已配置目录，自动跳过依赖和构建目录。"}, func(ctx adktool.Context, a projectGrepArgs) (out grepFilesResult, err error) {
		if err = projectPath(a.Path); err != nil {
			return
		}
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		err = store.WithRead(agent, a.Project, func(dir string, p codeproject.Project) error {
			out, err = grepFiles(NewScopedRoot(dir, p.Scope()), grepFilesArgs{Pattern: a.Pattern, Path: a.Path, Glob: a.Glob, MaxMatches: a.MaxMatches})
			return err
		})
		return
	})
	if err != nil {
		return nil, err
	}
	sd, err := functiontool.New(functiontool.Config{Name: "search_project_dirs", Description: "按业务名称、描述或标签查找项目里已标注的目录，返回仓库相对路径。用户说「优惠券服务」「所有支付相关的服务」时先用它定位目录，再去读代码核实。标注由用户维护，是业务背景而非已验证的结论；没有匹配结果不代表代码不存在，只代表没人标注过。"}, func(_ adktool.Context, a projectSearchArgs) (out searchDirsResult, err error) {
		err = store.WithRead(agent, a.Project, func(_ string, p codeproject.Project) error {
			out = searchDirs(p, a.Query, a.Tag, a.Limit)
			return nil
		})
		return
	})
	if err != nil {
		return nil, err
	}
	lt, err := functiontool.New(functiontool.Config{Name: "list_project_tags", Description: "列出项目里已使用的目录标签及各自的目录数量，用来了解这个仓库是怎么分类的。"}, func(_ adktool.Context, a projectTagArgs) (out map[string]any, err error) {
		err = store.WithRead(agent, a.Project, func(_ string, p codeproject.Project) error {
			counts := map[string]int{}
			for _, d := range p.Annotations() {
				for _, tag := range d.Tags {
					counts[tag]++
				}
			}
			tags := []map[string]any{}
			for tag, n := range counts {
				tags = append(tags, map[string]any{"tag": tag, "directories": n})
			}
			sort.Slice(tags, func(i, j int) bool {
				if tags[i]["directories"].(int) != tags[j]["directories"].(int) {
					return tags[i]["directories"].(int) > tags[j]["directories"].(int)
				}
				return tags[i]["tag"].(string) < tags[j]["tag"].(string)
			})
			out = map[string]any{"tags": tags, "annotated_directories": len(p.Annotations())}
			return nil
		})
		return
	})
	if err != nil {
		return nil, err
	}
	pr, err := functiontool.New(functiontool.Config{Name: "propose_project_dir_info", Description: "为项目里的目录提交业务名称、描述和标签的【草稿】，供用户在代码页面审核。适合被要求「给这些服务补上说明」时批量提交。\n这些草稿不会立即生效，也不会被其他工具读到——用户逐条确认或批量采纳后才进入正式标注。只能标注分析范围内、且快照里确实存在的目录。描述要写这个目录负责什么、边界在哪，依据是你实际读到的代码（README、入口文件、proto），不要臆测。"}, func(_ adktool.Context, a projectProposeArgs) (map[string]any, error) {
		if len(a.Directories) == 0 {
			return nil, fmt.Errorf("directories 不能为空")
		}
		if len(a.Directories) > 200 {
			return nil, fmt.Errorf("一次最多提交 200 个目录的草稿")
		}
		proposed := map[string]codeproject.DirectoryInfo{}
		// Validated inside WithRead so the scope check and the "does this path
		// exist" check run against the same snapshot the agent just read.
		err := store.WithRead(agent, a.Project, func(dir string, p codeproject.Project) error {
			roots := NewScopedRoot(dir, p.Scope())
			for _, d := range a.Directories {
				if err := projectPath(d.Path); err != nil {
					return err
				}
				abs, err := roots.Resolve(d.Path)
				if err != nil {
					return fmt.Errorf("%s：%w", d.Path, err)
				}
				if info, err := os.Stat(abs); err != nil || !info.IsDir() {
					return fmt.Errorf("%s 不是目录", d.Path)
				}
				proposed[strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(d.Path), "./"), "/")] = codeproject.DirectoryInfo{
					Name: d.Name, Description: d.Description, Tags: d.Tags,
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		// Outside WithRead: it holds the read lock, and writing takes the write one.
		n, err := store.ProposeAnnotations(a.Project, proposed)
		if err != nil {
			return nil, err
		}
		return map[string]any{"proposed": n, "status": "已提交草稿，等待用户在代码页面审核；在采纳前不会影响任何分析"}, nil
	})
	if err != nil {
		return nil, err
	}
	ck, err := functiontool.New(functiontool.Config{Name: "check_project_updates", Description: "现查远端分支头，确认这个项目的快照是不是还和远端一致。list_code_projects 已经带了这个结论（needs_sync），只有在用户刚说「我同步过了」、或者上一次核对失败需要重试时才用它——它每次都真的连一次远端。"}, func(ctx adktool.Context, a projectTagArgs) (map[string]any, error) {
		probeCtx, cancel := context.WithTimeout(ctx, remoteProbeBudget)
		defer cancel()
		st, err := store.CheckRemoteFor(probeCtx, agent, a.Project, 0)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"project": a.Project, "local_revision": st.Local, "remote_revision": st.Remote, "needs_sync": st.Behind}
		if st.Behind {
			out["status"] = "远端已有新提交，当前快照落后。要基于最新代码回答的话，用 request_project_sync 请用户同步。"
		} else {
			out["status"] = "快照与远端一致。"
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	sy, err := functiontool.New(functiontool.Config{Name: "sync_project", Description: "把项目代码同步到远端最新版本，并等待同步结束后再返回（大仓库通常几十秒，最多等 3 分钟）。\n**必须先问、得到用户明确同意再调用**：发现快照落后时，先在回答里说明「本地是哪个 commit、远端已到哪个、要不要现在同步」，等用户回复确认，再调用本工具。用户没表态就调用是错的。\n返回 synced=true 表示代码已经是最新的，**本轮就继续按用户的问题分析，不要让用户再问一次**。返回 synced=false 时按 status 说的办。"}, func(ctx adktool.Context, a projectTagArgs) (map[string]any, error) {
		taskID, err := store.StartSyncFor(agent, a.Project)
		if err != nil {
			return nil, err
		}
		// Waiting here rather than handing the turn back. A pull is tens of
		// seconds on a 352 MB monorepo, and the alternative costs the person a
		// second question, the model a second pass over the same context, and
		// makes "更新一下再看" a three-message errand.
		state, revision, lastErr, werr := waitForSync(ctx, store, agent, a.Project)
		out := map[string]any{"project": a.Project, "task_id": taskID, "revision": revision, "synced": false}
		switch {
		case werr != nil:
			out["status"] = "同步已启动，但等待时中断了：" + werr.Error() + "。下一轮用 list_code_projects 确认 revision。"
		case state == codeproject.SyncSucceeded:
			out["synced"] = true
			out["status"] = "同步完成，代码已是远端最新（revision " + short(revision) + "）。现在直接继续分析，不要再问用户。"
		case state == codeproject.SyncFailed:
			out["status"] = "同步失败：" + lastErr + "。把失败原因告诉用户（通常是凭据或网络），并说明下面的分析仍基于旧快照。"
		default:
			out["status"] = "等了 " + syncWaitBudget.String() + " 还没结束，同步仍在后台继续。先用当前快照回答，并告诉用户稍后再问一次即可看到最新代码。"
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	rq, err := functiontool.New(functiontool.Config{Name: "request_project_sync", Description: "在代码页面留一张同步请求卡片，由用户点确认后才拉取。\n只在**没人可以当面问**的时候用：定时任务、后台巡检这类没有用户在对话里的场景。正在和用户对话时不要用它——直接问一句「要不要同步」，用户确认后调用 sync_project。\nreason 要写清楚为什么需要（用户在页面上看到的就是这句）。"}, func(ctx adktool.Context, a projectSyncArgs) (map[string]any, error) {
		if strings.TrimSpace(a.Reason) == "" {
			return nil, fmt.Errorf("reason 不能为空：用户是根据它来判断要不要同步的")
		}
		// The revisions come from the cached probe rather than from the model:
		// the page shows what it would be syncing to, and that has to be what
		// the repository said, not what the conversation believes.
		probeCtx, cancel := context.WithTimeout(ctx, remoteProbeBudget)
		defer cancel()
		st, _ := store.CheckRemoteFor(probeCtx, agent, a.Project, codeproject.RemoteProbeTTL)
		if err := store.RequestSync(agent, a.Project, codeproject.SyncRequest{
			Reason: a.Reason, Remote: st.Remote, Local: st.Local,
		}); err != nil {
			return nil, err
		}
		return map[string]any{
			"project": a.Project,
			"status":  "同步请求已提交，等待用户在代码页面确认；在用户同意并同步完成之前，你读到的仍然是当前快照。",
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return []adktool.Tool{ls, rd, ld, gp, sd, lt, pr, ck, sy, rq}, nil
}

// waitForSync blocks until the pull leaves the running states, the budget runs
// out, or the turn is cancelled. Polling rather than a channel because the
// state is a file that another process may also be writing.
func waitForSync(ctx context.Context, store *codeproject.Store, agent, project string) (state, revision, lastErr string, err error) {
	deadline := time.Now().Add(syncWaitBudget)
	for {
		state, revision, lastErr, err = store.SyncStateFor(agent, project)
		if err != nil {
			return
		}
		if state != codeproject.SyncQueued && state != codeproject.SyncRunning {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return state, revision, lastErr, ctx.Err()
		case <-time.After(syncPollEvery):
		}
	}
}

// syncWaitBudget is how long a turn will hold for a pull. A full monorepo
// clone is tens of seconds; past this it is better to answer from the old
// snapshot than to leave the person watching a spinner with nothing to read.
const (
	syncWaitBudget = 3 * time.Minute
	syncPollEvery  = 2 * time.Second
)

// remoteProbeBudget bounds how long an agent waits to learn whether a snapshot
// is current. Past this the answer is "未能核对远端", which is a usable answer;
// a turn that hangs on someone's git server is not.
const remoteProbeBudget = 8 * time.Second

type projectSyncArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
	Reason  string `json:"reason" jsonschema:"为什么需要同步，用户会看到这句话"`
}

type proposedDir struct {
	Path        string   `json:"path" jsonschema:"仓库相对目录，如 services/order"`
	Name        string   `json:"name,omitempty" jsonschema:"业务名称，如 订单服务"`
	Description string   `json:"description,omitempty" jsonschema:"这个目录负责什么、边界在哪，依据实际读到的代码"`
	Tags        []string `json:"tags,omitempty" jsonschema:"分类标签，如 [支付, 核心链路]"`
}
type projectProposeArgs struct {
	Project     string        `json:"project" jsonschema:"已分配项目的标识"`
	Directories []proposedDir `json:"directories" jsonschema:"要提交草稿的目录，一次最多 200 个"`
}

type projectSearchArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
	Query   string `json:"query,omitempty" jsonschema:"业务名称或描述里的关键词，如 优惠券"`
	Tag     string `json:"tag,omitempty" jsonschema:"按标签精确筛选，如 支付"`
	Limit   int    `json:"limit,omitempty" jsonschema:"最多返回多少个目录，默认 50"`
}
type projectTagArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
}
type searchDirsResult struct {
	Directories []codeproject.Directory `json:"directories"`
	Total       int                     `json:"total"`
	Truncated   bool                    `json:"truncated,omitempty"`
	Note        string                  `json:"note,omitempty"`
}

// annotateEntries attaches the operator's labels to a directory listing. The
// listing is where the labels earn their keep: a model browsing services/ sees
// what each one is for instead of 130 names it would have to open to identify.
func annotateEntries(out *listDirResult, p codeproject.Project, requested string) {
	base := strings.Trim(strings.TrimSpace(requested), "/")
	if base == "." {
		base = ""
	}
	for i := range out.Entries {
		if !out.Entries[i].Dir {
			continue
		}
		rel := out.Entries[i].Name
		if base != "" {
			rel = base + "/" + rel
		}
		if info, ok := p.Annotation(rel); ok {
			out.Entries[i].Label, out.Entries[i].Note, out.Entries[i].Tags = info.Name, info.Description, info.Tags
		}
	}
}

// searchDirs matches annotations, never file contents — that is what
// grep_project_files is for. A tag filter is exact; a query is a
// case-insensitive substring over name, description and path.
func searchDirs(p codeproject.Project, query, tag string, limit int) searchDirsResult {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := strings.ToLower(strings.TrimSpace(query))
	tag = strings.TrimSpace(tag)
	matches := []codeproject.Directory{}
	for _, d := range p.Annotations() {
		if tag != "" {
			found := false
			for _, t := range d.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if q != "" {
			hay := strings.ToLower(d.Path + " " + d.Name + " " + d.Description + " " + strings.Join(d.Tags, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		matches = append(matches, d)
	}
	res := searchDirsResult{Total: len(matches)}
	if len(matches) > limit {
		res.Directories, res.Truncated = matches[:limit], true
	} else {
		res.Directories = matches
	}
	if len(matches) == 0 {
		res.Note = "没有标注匹配。这只说明没人标注过，不代表相关代码不存在——可以改用 grep_project_files 搜代码。"
	}
	return res
}

// humanAge renders a snapshot's age the way a person would say it, so the model
// does not have to do date arithmetic on a timestamp to decide whether the code
// it is reading is stale.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚刚同步"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前同步", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前同步", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前同步", int(d.Hours()/24))
	}
}
