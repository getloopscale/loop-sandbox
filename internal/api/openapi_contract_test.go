package api_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

const openAPIPath = "../../api/openapi.yaml"

var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true, "patch": true, "head": true, "options": true,
}

// documentedOperations returns "METHOD /path" for every operation in the OpenAPI
// document, mapped to its operation node.
func documentedOperations(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	out := map[string]map[string]any{}
	for path, item := range doc.Paths {
		for method, op := range item {
			if !httpMethods[method] {
				continue // path-level "parameters", "summary", ...
			}
			node, ok := op.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: operation is not a mapping", method, path)
			}
			out[strings.ToUpper(method)+" "+path] = node
		}
	}
	return out
}

// registeredOperations returns "METHOD /path" for every route the router serves,
// in OpenAPI path syntax ("{path...}" becomes "{path}").
func registeredOperations() map[string]api.Route {
	out := map[string]api.Route{}
	for _, route := range append(api.Routes(), api.MetricsRoute) {
		out[route.Method+" "+strings.ReplaceAll(route.Path, "...}", "}")] = route
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestEveryRegisteredRouteIsDocumentedInOpenAPI(t *testing.T) {
	documented := documentedOperations(t)

	for _, op := range sortedKeys(registeredOperations()) {
		if _, ok := documented[op]; !ok {
			t.Errorf("route %q is served by the router and missing from %s", op, openAPIPath)
		}
	}
}

func TestEveryOpenAPIOperationIsServedByTheRouter(t *testing.T) {
	registered := registeredOperations()

	for _, op := range sortedKeys(documentedOperations(t)) {
		if _, ok := registered[op]; !ok {
			t.Errorf("operation %q is documented in %s and not served by the router", op, openAPIPath)
		}
	}
}

func TestEveryOpenAPIOperationHasAUniqueOperationID(t *testing.T) {
	seen := map[string]string{}

	documented := documentedOperations(t)
	for _, op := range sortedKeys(documented) {
		id, _ := documented[op]["operationId"].(string)
		if id == "" {
			t.Errorf("operation %q has no operationId", op)
			continue
		}
		if other, dup := seen[id]; dup {
			t.Errorf("operationId %q is used by both %q and %q", id, other, op)
		}
		seen[id] = op
	}
}

func TestPublicRoutesAreTheOnlyOperationsWithoutSecurity(t *testing.T) {
	documented := documentedOperations(t)

	for op, route := range registeredOperations() {
		node, ok := documented[op]
		if !ok {
			continue // reported by TestEveryRegisteredRouteIsDocumentedInOpenAPI
		}
		security, overridden := node["security"].([]any)
		anonymous := overridden && len(security) == 0
		if route.Public != anonymous {
			t.Errorf("%q: router public=%v, OpenAPI anonymous (security: [])=%v", op, route.Public, anonymous)
		}
	}
}

func TestOpenAPICapabilityNamesAreTheOnesTheRuntimeKnows(t *testing.T) {
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Capabilities struct {
					Properties struct {
						Capabilities struct {
							Items struct {
								Properties struct {
									Name struct {
										Enum []string `yaml:"enum"`
									} `yaml:"name"`
								} `yaml:"properties"`
							} `yaml:"items"`
						} `yaml:"capabilities"`
					} `yaml:"properties"`
				} `yaml:"Capabilities"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	documented := doc.Components.Schemas.Capabilities.Properties.Capabilities.Items.Properties.Name.Enum
	var known []string
	for _, c := range runtime.AllCapabilities() {
		known = append(known, string(c))
	}

	sort.Strings(documented)
	sort.Strings(known)

	if strings.Join(documented, ",") != strings.Join(known, ",") {
		t.Errorf("capability names in the OpenAPI document:\n  %v\nnames the runtime reports:\n  %v", documented, known)
	}
}
