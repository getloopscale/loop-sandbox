package templates_test

import (
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/templates"
)

func TestOfficial_has_at_least_three_vetted_templates(t *testing.T) {
	cat := templates.Official()
	if len(cat) < 3 {
		t.Fatalf("official templates = %d, want >= 3", len(cat))
	}
	seen := map[string]bool{}
	for _, tpl := range cat {
		if tpl.Name == "" || tpl.Image == "" || tpl.Description == "" {
			t.Errorf("incomplete template: %+v", tpl)
		}
		if seen[tpl.Name] {
			t.Errorf("duplicate template name %q", tpl.Name)
		}
		seen[tpl.Name] = true
	}
}

func TestImages_matches_catalog_and_pins_tags(t *testing.T) {
	imgs := templates.Images()
	if len(imgs) != len(templates.Official()) {
		t.Fatalf("Images() = %d, want %d (one per template)", len(imgs), len(templates.Official()))
	}
	for _, img := range imgs {
		// A pinned tag (has ':') so tenants never run a floating/unpinned image.
		if !containsColon(img) {
			t.Errorf("image %q is not tag-pinned", img)
		}
	}
}

func containsColon(s string) bool {
	for i := range s {
		if s[i] == ':' {
			return true
		}
	}
	return false
}
