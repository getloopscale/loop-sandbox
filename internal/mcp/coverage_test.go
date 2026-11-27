package mcp_test

import (
	"slices"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/mcp"
)

// restRoutes returns every route of the REST API as "METHOD /path".
func restRoutes() []string {
	routes := []string{api.MetricsRoute.Method + " " + api.MetricsRoute.Path}
	for _, r := range api.Routes() {
		routes = append(routes, r.Method+" "+r.Path)
	}
	return routes
}

func TestEveryRESTRouteHasAToolOrAStatedReason(t *testing.T) {
	covered := map[string]bool{}
	for _, routes := range mcp.Coverage() {
		for _, r := range routes {
			covered[r] = true
		}
	}

	for _, route := range restRoutes() {
		_, excluded := mcp.RoutesWithoutTool[route]
		switch {
		case !covered[route] && !excluded:
			t.Errorf("%s has no MCP tool: add one in internal/mcp/tools.go, or add the route to RoutesWithoutTool with the reason", route)
		case covered[route] && excluded:
			t.Errorf("%s has a tool and is also listed in RoutesWithoutTool", route)
		}
	}
}

func TestToolCoverageNamesOnlyRoutesThatExist(t *testing.T) {
	existing := restRoutes()

	for tool, routes := range mcp.Coverage() {
		if len(routes) == 0 {
			t.Errorf("tool %s covers no route", tool)
		}
		for _, r := range routes {
			if !slices.Contains(existing, r) {
				t.Errorf("tool %s claims %q, which is not a route of the API", tool, r)
			}
		}
	}
	for route, reason := range mcp.RoutesWithoutTool {
		if !slices.Contains(existing, route) {
			t.Errorf("RoutesWithoutTool lists %q, which is not a route of the API", route)
		}
		if reason == "" {
			t.Errorf("RoutesWithoutTool lists %q without a reason", route)
		}
	}
}

func TestEveryToolWithCoverageIsGrantedByAScope(t *testing.T) {
	granted := mcp.AllowedTools([]string{mcp.ScopeExecute})

	for tool := range mcp.Coverage() {
		if !granted[tool] {
			t.Errorf("tool %s is in the coverage table and no scope grants it, so it cannot be called", tool)
		}
	}
	if len(granted) != len(mcp.Coverage()) {
		t.Errorf("%d tools are granted and %d have coverage; every tool must state the routes it stands for", len(granted), len(mcp.Coverage()))
	}
}
