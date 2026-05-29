package tool

import adktool "google.golang.org/adk/tool"

// Builtins returns the built-in tool instances registered for agents.
//
// Phase 2 first batch: only web_search. fetch_url / run_code / read_file /
// write_file / query_db land later, behind the permission model (see PLAN §8
// risk 6).
func Builtins() ([]adktool.Tool, error) {
	ws, err := NewWebSearchTool()
	if err != nil {
		return nil, err
	}
	return []adktool.Tool{ws}, nil
}
