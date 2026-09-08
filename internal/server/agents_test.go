package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
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
