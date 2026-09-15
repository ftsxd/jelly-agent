package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func TestAgentRequiredToolsAndSuitesRoundTrip(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	s.engine().Config().SourcePath = path
	s = s.WithConfigPath(path)

	body := `{"name":"MetricsQuery","description":"查询指标",` +
		`"required_tools":["query_instant","query_range","query_instant"],` +
		`"required_suites":["promql","promql"],"enabled":true}`
	if w := do(t, s, "POST", "/api/agents", body); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}

	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Agents) != 1 {
		t.Fatalf("agents = %+v", raw.Agents)
	}
	got := raw.Agents[0]
	if !slices.Equal(got.RequiredTools, []string{"query_instant", "query_range"}) ||
		!slices.Equal(got.RequiredSuites, []string{"promql"}) {
		t.Fatalf("required capability did not round trip: %+v", got)
	}

	w := do(t, s, "GET", "/api/agents", "")
	var listed struct {
		Agents []config.AgentDef `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Agents) != 1 || !slices.Equal(listed.Agents[0].RequiredTools, got.RequiredTools) {
		t.Fatalf("list omitted required capability: %+v", listed.Agents)
	}
}

// The HTTP layer has to preserve the same three states the config does. JSON
// makes this easy to get wrong: a plain []string decodes both `null` and `[]`
// to nil, which would turn "this coordinator gets no skills" into "it gets all
// of them" — a silent widening, on the one agent where it matters most.
func TestAgentSkillsTriStateRoundTripsThroughAPI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want func(*[]string) bool
		desc string
	}{
		{
			name: "Unset", body: `{"name":"Unset","description":"d","enabled":true}`,
			want: func(s *[]string) bool { return s == nil },
			desc: "字段缺席应保持 nil（= 全部技能）",
		},
		{
			name: "Null", body: `{"name":"Null","description":"d","skills":null,"enabled":true}`,
			want: func(s *[]string) bool { return s == nil },
			desc: "显式 null 应保持 nil（= 全部技能）",
		},
		{
			name: "None", body: `{"name":"None","description":"d","skills":[],"enabled":true}`,
			want: func(s *[]string) bool { return s != nil && len(*s) == 0 },
			desc: "空数组应保持空数组（= 没有技能）",
		},
		{
			name: "Some", body: `{"name":"Some","description":"d","skills":["a"," a ","b",""],"enabled":true}`,
			want: func(s *[]string) bool { return s != nil && slices.Equal(*s, []string{"a", "b"}) },
			desc: "列表应去重去空白后保留",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			s.engine().Config().SourcePath = path
			s = s.WithConfigPath(path)

			if w := do(t, s, "POST", "/api/agents", c.body); w.Code != http.StatusOK {
				t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
			}
			raw, err := config.LoadRaw(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw.Agents) != 1 {
				t.Fatalf("agents = %+v", raw.Agents)
			}
			if !c.want(raw.Agents[0].Skills) {
				t.Fatalf("%s —— 落盘后是 %v", c.desc, raw.Agents[0].Skills)
			}

			// ...and the same state has to come back out of GET, or the form
			// would re-save an agent under a different policy than it has.
			w := do(t, s, "GET", "/api/agents", "")
			var listed struct {
				Agents []config.AgentDef `json:"agents"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Agents) != 1 || !c.want(listed.Agents[0].Skills) {
				t.Fatalf("%s —— GET 返回的是 %v", c.desc, listed.Agents[0].Skills)
			}
		})
	}
}

// TestAgentVarsMasked verifies per-agent variables persist (in config), that
// the API never echoes their values — only the key names — and that they are
// not collateral damage of the console re-posting a partial agent record.
func TestAgentVarsMasked(t *testing.T) {
	s := newEmptyServer(t)
	if w := do(t, s, "POST", "/api/agents", `{"name":"ops","description":"运维","enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("create agent: %d %s", w.Code, w.Body.String())
	}

	w := do(t, s, "POST", "/api/agents/ops/vars", `{"vars":{"K8S_TOKEN":"super-secret-xyz"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set vars status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().Config().AgentVars["ops"]["K8S_TOKEN"]; got != "super-secret-xyz" {
		t.Fatalf("var not persisted: %q", got)
	}

	// The list marshals AgentDef verbatim, so this is the assertion that keeps
	// the value off the wire.
	w = do(t, s, "GET", "/api/agents", "")
	body := w.Body.String()
	if strings.Contains(body, "super-secret-xyz") {
		t.Fatalf("var value leaked: %s", body)
	}
	if !strings.Contains(body, `"var_keys":{"ops":["K8S_TOKEN"]}`) {
		t.Fatalf("var key not surfaced: %s", body)
	}

	// Toggling the agent off re-posts a partial record. Variables live in their
	// own config map precisely so that click cannot erase them.
	if w := do(t, s, "POST", "/api/agents", `{"name":"ops","description":"运维","enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("toggle agent: %d", w.Code)
	}
	if got := s.engine().Config().AgentVars["ops"]["K8S_TOKEN"]; got != "super-secret-xyz" {
		t.Fatalf("saving the agent wiped its variables: %q", got)
	}

	if w := do(t, s, "DELETE", "/api/agents/ops/vars/K8S_TOKEN", ""); w.Code != http.StatusOK {
		t.Fatalf("delete var: %d", w.Code)
	}
	if _, ok := s.engine().Config().AgentVars["ops"]; ok {
		t.Fatalf("agent vars not cleaned up after last delete")
	}
}

// An empty value means "keep what is stored", so the console can render the
// key without ever holding the secret.
func TestAgentVarsBlankValueKeepsStored(t *testing.T) {
	s := newEmptyServer(t)
	if w := do(t, s, "POST", "/api/agents", `{"name":"ops","enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("create agent: %d", w.Code)
	}
	if w := do(t, s, "POST", "/api/agents/ops/vars", `{"vars":{"K8S_TOKEN":"keep-me"}}`); w.Code != http.StatusOK {
		t.Fatalf("set vars: %d", w.Code)
	}
	if w := do(t, s, "POST", "/api/agents/ops/vars", `{"vars":{"K8S_TOKEN":"","CLUSTER":"prod"}}`); w.Code != http.StatusOK {
		t.Fatalf("re-save vars: %d", w.Code)
	}
	vars := s.engine().Config().AgentVars["ops"]
	if vars["K8S_TOKEN"] != "keep-me" || vars["CLUSTER"] != "prod" {
		t.Fatalf("blank value did not preserve the stored secret: %+v", vars)
	}
}

func TestAgentVarsRejectsUnusableNames(t *testing.T) {
	s := newEmptyServer(t)
	if w := do(t, s, "POST", "/api/agents", `{"name":"ops","enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("create agent: %d", w.Code)
	}
	for _, body := range []string{
		`{"vars":{"PATH":"/evil/bin"}}`, // scrubEnv sets it; an override costs the script its interpreter
		`{"vars":{"HOME":"/tmp"}}`,      // likewise reserved
		`{"vars":{"2FA":"x"}}`,          // not exportable
		`{"vars":{"WITH-DASH":"x"}}`,    // not exportable
	} {
		if w := do(t, s, "POST", "/api/agents/ops/vars", body); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want 400", body, w.Code)
		}
	}
	if _, ok := s.engine().Config().AgentVars["ops"]; ok {
		t.Error("a rejected request still wrote to the config")
	}

	// And an agent that does not exist has nowhere to put them.
	if w := do(t, s, "POST", "/api/agents/ghost/vars", `{"vars":{"A":"b"}}`); w.Code != http.StatusNotFound {
		t.Errorf("vars for unknown agent status = %d, want 404", w.Code)
	}
}
