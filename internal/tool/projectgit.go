package tool

import (
	"fmt"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// The history half of code analysis.
//
// "这个服务最近改了什么"、"这行是谁什么时候加的"、"这两个版本差在哪" are not
// exotic questions — they are most of what anyone asks about someone else's
// service, and until these tools existed the honest answer was 我的工具只能读
// 文件。 Every one of them is answered from the object cache the sync already
// keeps, so none of this reaches the network or needs a credential.
//
// Same three guarantees as the file tools: the agent identity is captured at
// construction and never taken from arguments, the grant is rechecked inside
// every call, and the range limit is enforced in the store rather than asked
// for in a prompt — a diff is file content, and a diff outside the configured
// directories would hand over exactly what list_project_dir refuses to list.

type projectLogArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
	Path    string `json:"path,omitempty" jsonschema:"只看这个目录或文件的提交，仓库相对路径；留空是项目配置的全部目录"`
	Limit   int    `json:"limit,omitempty" jsonschema:"最多返回多少个提交，默认 20，上限 100"`
	Since   string `json:"since,omitempty" jsonschema:"只看这个时间之后的提交，如 2026-08-01 或 2 weeks ago"`
	Author  string `json:"author,omitempty" jsonschema:"按作者过滤，子串匹配"`
	Keyword string `json:"keyword,omitempty" jsonschema:"按提交信息过滤，不区分大小写"`
	Files   bool   `json:"with_files,omitempty" jsonschema:"顺带列出每个提交改了哪些文件"`
}

type projectShowArgs struct {
	Project  string `json:"project" jsonschema:"已分配项目的标识"`
	Revision string `json:"revision,omitempty" jsonschema:"提交 id，从 log_project_commits 的结果里取；留空为当前快照那个提交"`
	Path     string `json:"path,omitempty" jsonschema:"只看这个目录或文件的改动，仓库相对路径"`
	Patch    bool   `json:"with_patch,omitempty" jsonschema:"是否带上具体代码改动（diff）；只想知道改了哪些文件就别开"`
}

type projectDiffArgs struct {
	Project string `json:"project" jsonschema:"已分配项目的标识"`
	From    string `json:"from" jsonschema:"较旧的那个提交 id"`
	To      string `json:"to,omitempty" jsonschema:"较新的那个提交 id；留空为当前快照那个提交"`
	Path    string `json:"path,omitempty" jsonschema:"只比较这个目录或文件，仓库相对路径"`
	Patch   bool   `json:"with_patch,omitempty" jsonschema:"是否带上具体代码改动（diff）；先看文件清单再决定要不要开"`
}

// ProjectHistoryTools builds the log/show/diff trio for one agent identity.
func ProjectHistoryTools(store *codeproject.Store, agent string) ([]adktool.Tool, error) {
	lg, err := functiontool.New(functiontool.Config{Name: "log_project_commits", Description: "查看已分配项目的提交历史（git log），默认按时间倒序。用它回答「最近有什么更新」「这个目录谁在改」——先用 path 缩到具体服务目录，再看提交。\n返回的 snapshot_revision 是当前快照那个提交，历史就是从它往回数的。**local_history_commits 是本地存了多少个提交**：shallow=true 时更早的提交不在本地，提交数偏少是同步深度造成的，不代表仓库只有这么多提交——这一点要照实说，别把它讲成仓库的全部历史。\n和 list_code_projects 的 needs_sync 是两件事：那个说的是远端有没有更新，这个说的是本地这份历史里有什么。"}, func(ctx adktool.Context, a projectLogArgs) (out codeproject.LogResult, err error) {
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		return store.LogFor(ctx, agent, a.Project, codeproject.LogQuery{
			Path: a.Path, Limit: a.Limit, Since: a.Since,
			Author: a.Author, Keyword: a.Keyword, WithFiles: a.Files,
		})
	})
	if err != nil {
		return nil, err
	}
	sh, err := functiontool.New(functiontool.Config{Name: "show_project_commit", Description: "看一个提交具体做了什么：作者、时间、提交信息、改了哪些文件，以及可选的代码改动。revision 从 log_project_commits 的结果里取。\n先不带 with_patch 看文件清单，确认是想找的那个提交，再对具体目录开 with_patch——整个提交的 diff 经常大到没法读，也会把上下文吃光。patch 超长会被截断并在 note 里说明，这时用 path 缩小范围重读。"}, func(ctx adktool.Context, a projectShowArgs) (out codeproject.ShowResult, err error) {
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		return store.ShowFor(ctx, agent, a.Project, codeproject.ShowQuery{Revision: a.Revision, Path: a.Path, Patch: a.Patch})
	})
	if err != nil {
		return nil, err
	}
	df, err := functiontool.New(functiontool.Config{Name: "diff_project_revisions", Description: "比较两个提交之间改了什么（git diff），用于「上线前后差了哪些代码」「这个版本相对上个版本动了什么」。from 是较旧的那个，to 留空表示当前快照。\n两个提交都必须在本地历史里；报「本地历史里没有这个版本」说明它早于同步深度，不是提交不存在。同样先看文件清单，再按 path 开 with_patch。"}, func(ctx adktool.Context, a projectDiffArgs) (out codeproject.DiffResult, err error) {
		if strings.TrimSpace(a.From) == "" {
			return out, fmt.Errorf("from 不能为空：要比较的旧版本提交 id，从 log_project_commits 里取")
		}
		if err = gate.check(ctx.SessionID(), ctx.InvocationID(), a.Project); err != nil {
			return
		}
		return store.DiffFor(ctx, agent, a.Project, codeproject.DiffQuery{From: a.From, To: a.To, Path: a.Path, Patch: a.Patch})
	})
	if err != nil {
		return nil, err
	}
	return []adktool.Tool{lg, sh, df}, nil
}
