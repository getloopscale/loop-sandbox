package templates_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/templates"
)

func TestTheCatalogHasTheAgentAndDesktopTemplates(t *testing.T) {
	have := map[string]bool{}
	for _, tpl := range templates.Official() {
		have[tpl.Name] = true
	}
	for _, name := range []string{"base", "claude", "codex", "gemini", "opencode", "openclaw", "droid", "amp", "desktop", "browser"} {
		if !have[name] {
			t.Errorf("the catalog has no template %q", name)
		}
	}
}

// A template whose image this repository builds must be one the templates workflow
// publishes, at the version the workflow publishes. Otherwise the catalog names an
// image that does not exist and the sandbox fails at the pull.
func TestEveryBuiltTemplateIsPublishedByTheWorkflow(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/templates.yml")
	if err != nil {
		t.Fatalf("read the templates workflow: %v", err)
	}
	workflow := string(raw)
	version := regexp.MustCompile(`TEMPLATES_VERSION: "([^"]+)"`).FindStringSubmatch(workflow)
	if version == nil {
		t.Fatal("the templates workflow sets no TEMPLATES_VERSION")
	}
	const prefix = "ghcr.io/getloopscale/loop-sandbox:template-"
	built := 0
	for _, tpl := range templates.Official() {
		if !strings.HasPrefix(tpl.Image, prefix) {
			continue
		}
		built++
		if want := prefix + tpl.Name + "-" + version[1]; tpl.Image != want {
			t.Errorf("template %q runs %s; the workflow publishes %s", tpl.Name, tpl.Image, want)
		}
		if !strings.Contains(workflow, "{ name: "+tpl.Name+",") {
			t.Errorf("the templates workflow builds no image named %q", tpl.Name)
		}
	}
	if built != 9 {
		t.Errorf("%d templates run an image built here; want 9", built)
	}
}
