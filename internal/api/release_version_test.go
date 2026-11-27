package api

import (
	"os"
	"regexp"
	"testing"
)

// declaredVersion returns the first submatch of pattern in the file at path.
func declaredVersion(t *testing.T, path, pattern string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s declares no version matching %s", path, pattern)
	}
	return string(m[1])
}

// The server, the TypeScript SDK and the Python SDK are released together, so a
// user can tell from one number which client matches which server.
func TestServerAndSDKsDeclareTheSameVersion(t *testing.T) {
	api := declaredVersion(t, "../../api/openapi.yaml", `(?m)^  version: (\S+)$`)
	js := declaredVersion(t, "../../sdk/javascript/package.json", `(?m)^  "version": "([^"]+)"`)
	py := declaredVersion(t, "../../sdk/python/pyproject.toml", `(?m)^version = "([^"]+)"`)

	if js != api || py != api {
		t.Errorf("versions differ: openapi=%s typescript=%s python=%s", api, js, py)
	}
}
