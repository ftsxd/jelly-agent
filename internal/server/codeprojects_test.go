package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
)

func TestCodeProjectAPI(t *testing.T) {
	s, _ := codeServer(t)
	create := `{"id":"orders","name":"订单服务","url":"https://git.example.com/orders.git","branch":"main"}`
	if w := do(t, s, "POST", "/api/code-projects", create); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(t, s, "PUT", "/api/code-projects/orders/grants", `{"grants":[{"agent":"missing"}]}`); w.Code != 400 {
		t.Fatal("unknown agent accepted", w.Code)
	}
	if w := do(t, s, "PUT", "/api/code-projects/orders/grants", `{"grants":[{"agent":"root"}]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// An edit made from stale UI state must not clobber independently saved grants.
	if w := do(t, s, "POST", "/api/code-projects", create); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := do(t, s, "GET", "/api/code-projects", "")
	var payload struct {
		Projects []codeproject.Project `json:"projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Projects) != 1 || len(payload.Projects[0].Grants) != 1 || payload.Projects[0].Grants[0].Agent != "root" {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "snapshot") {
		t.Fatal("snapshot path leaked")
	}
	if w := do(t, s, "PUT", "/api/code-projects/orders/grants", `{"grants":[]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := do(t, s, "DELETE", "/api/code-projects/orders", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := do(t, s, "POST", "/api/code-projects/orders/sync", `{}`); w.Code != http.StatusNotFound {
		t.Fatal("missing sync", w.Code)
	}
}
func TestCodeProjectsUseExistingAdminAuthentication(t *testing.T) {
	s := newAdminServer(t)
	for _, route := range []struct{ method, path string }{{"GET", "/api/code-projects"}, {"POST", "/api/code-projects"}, {"DELETE", "/api/code-projects/x"}, {"PUT", "/api/code-projects/x/grants"}, {"POST", "/api/code-projects/x/sync"}} {
		if w := do(t, s, route.method, route.path, `{}`); w.Code != 401 {
			t.Fatalf("unauthed %s: %d", route.path, w.Code)
		}
	}
}

// The whole reason a real repository could not be pulled: the clone ran inside
// the request on r.Context(), so the browser tab, the proxy or the read timeout
// cancelled it. The handler must hand back a task id and let the pull outlive
// the request.
func TestSyncReturnsATaskWithoutWaitingForThePull(t *testing.T) {
	s, _ := codeServer(t)
	create := `{"id":"orders","name":"订单服务","url":"https://127.0.0.1:1/orders.git","branch":"main"}`
	if w := do(t, s, "POST", "/api/code-projects", create); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	w := do(t, s, "POST", "/api/code-projects/orders/sync", `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("sync did not accept the task: %d %s", w.Code, w.Body.String())
	}
	var first struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || first.TaskID == "" {
		t.Fatalf("no task id returned: %s", w.Body.String())
	}

	// Pressing the button again returns the same task instead of starting a
	// second clone of the same repository.
	w = do(t, s, "POST", "/api/code-projects/orders/sync", `{}`)
	var second struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusAccepted || second.TaskID != first.TaskID {
		t.Fatalf("duplicate sync started a second task: %d %s", w.Code, w.Body.String())
	}
}

// Directory configuration round-trips through the API, and a description that
// names a directory the project does not have is refused — metadata must never
// become a second way to name a path.
func TestCodeProjectDirectoriesRoundTripAndRejectStrayMetadata(t *testing.T) {
	s, _ := codeServer(t)
	good := `{"id":"coupon","name":"优惠券","url":"https://git.example.com/silkworm.git","branch":"master",
	  "root_path":"services/discount_coupon/","reference_paths":["common/"],
	  "directory_metadata":{"services/discount_coupon":{"name":"优惠券服务","description":"发放与核销。"}}}`
	if w := do(t, s, "POST", "/api/code-projects", good); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := do(t, s, "GET", "/api/code-projects", "")
	var payload struct {
		Projects []codeproject.Project `json:"projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	p := payload.Projects[0]
	if p.RootPath != "services/discount_coupon" || len(p.ReferencePaths) != 1 || p.ReferencePaths[0] != "common" {
		t.Fatalf("directories not normalized: %+v", p)
	}
	if p.DirectoryMeta["services/discount_coupon"].Name != "优惠券服务" {
		t.Fatalf("directory description lost: %+v", p.DirectoryMeta)
	}

	for _, bad := range []string{
		`{"id":"coupon","name":"优惠券","url":"https://git.example.com/silkworm.git","branch":"master","root_path":"/etc"}`,
		`{"id":"coupon","name":"优惠券","url":"https://git.example.com/silkworm.git","branch":"master","reference_paths":["../../etc"]}`,
		`{"id":"coupon","name":"优惠券","url":"https://git.example.com/silkworm.git","branch":"master","root_path":"services/a","directory_metadata":{"services/b":{"name":"别人"}}}`,
	} {
		if w := do(t, s, "POST", "/api/code-projects", bad); w.Code != 400 {
			t.Fatalf("invalid directory configuration accepted: %d %s", w.Code, bad)
		}
	}
}

// The console must be able to save a credential without shell access to the
// server — in a container the environment variable is visible to anyone who can
// docker inspect or exec in, so it is the weaker option, not the safer one.
// What the API must never do is hand the value back.
func TestTokenIsWriteOnlyThroughTheAPI(t *testing.T) {
	s, _ := codeServer(t)
	const secret = "tok-MUST-NOT-COME-BACK"
	create := `{"id":"silkworm","name":"silkworm","url":"https://e.coding.net/x/y/silkworm.git","branch":"master","username":"CI-CD-OPS","token":"` + secret + `"}`
	if w := do(t, s, "POST", "/api/code-projects", create); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	w := do(t, s, "GET", "/api/code-projects", "")
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("GET returned the token: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatalf("console cannot tell a credential is configured: %s", w.Body.String())
	}

	// An edit that omits the token keeps it: the console can never re-send a
	// value it is not allowed to read.
	edit := `{"id":"silkworm","name":"silkworm 改名","url":"https://e.coding.net/x/y/silkworm.git","branch":"master","username":"CI-CD-OPS"}`
	if w := do(t, s, "POST", "/api/code-projects", edit); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = do(t, s, "GET", "/api/code-projects", "")
	if !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatalf("an unrelated edit dropped the credential: %s", w.Body.String())
	}

	// Clearing has to be explicit.
	clear := `{"id":"silkworm","name":"silkworm","url":"https://e.coding.net/x/y/silkworm.git","branch":"master","clear_token":true}`
	if w := do(t, s, "POST", "/api/code-projects", clear); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = do(t, s, "GET", "/api/code-projects", "")
	if strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatalf("clear_token did not remove the credential: %s", w.Body.String())
	}
}

// The button is a shortcut for typing the prompt, not a way around assignment:
// the run executes as the assigned agent, so the annotation tool sees exactly
// the grant an ordinary analysis would.
func TestAnnotateRequiresAnAssignedAgentAndASyncedSnapshot(t *testing.T) {
	s, _ := codeServer(t)
	create := `{"id":"orders","name":"订单服务","url":"https://git.example.com/orders.git","branch":"main"}`
	if w := do(t, s, "POST", "/api/code-projects", create); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// Unassigned: refused before anything is spent.
	w := do(t, s, "POST", "/api/code-projects/orders/annotate", `{}`)
	if w.Code == http.StatusAccepted {
		t.Fatal("started a label run for an unassigned project")
	}
	if !strings.Contains(w.Body.String(), "分配") {
		t.Fatalf("refusal does not say what to do: %s", w.Body.String())
	}

	// Assigned but never synced: also refused, because there is nothing to read.
	if w := do(t, s, "PUT", "/api/code-projects/orders/grants", `{"grants":[{"agent":"root"}]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = do(t, s, "POST", "/api/code-projects/orders/annotate", `{}`)
	if w.Code == http.StatusAccepted {
		t.Fatal("started a label run with no snapshot to read")
	}
	if !strings.Contains(w.Body.String(), "同步") {
		t.Fatalf("refusal does not say what to do: %s", w.Body.String())
	}
}

// The prompt has to steer toward a few batch searches: opening 130 service
// directories one at a time is what makes this expensive.
func TestAnnotatePromptSteersTowardBatchedSearches(t *testing.T) {
	got := annotatePrompt(codeproject.Project{ID: "silkworm", Name: "Silkworm", RootPath: "services"})
	for _, want := range []string{"silkworm", "Silkworm", "services", "grep_project_files", "propose_project_dir_info", "草稿", "不要逐个目录翻文件"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !strings.Contains(got, "跳过") {
		t.Error("prompt does not tell the agent to skip what it cannot justify")
	}
}
