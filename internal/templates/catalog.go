// Package templates holds the built-in catalog of vetted base images a sandbox
// can be created from (M5). The catalog doubles as the container driver's default
// image allow-list: untrusted tenants may only run these pinned images.
package templates

// Template is an approved base image exposed via GET /v1/templates.
type Template struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Description string `json:"description"`
}

// Official is the built-in catalog. Names are stable identifiers; images are
// pinned by tag so a sandbox never runs a floating "latest" of a language runtime.
// busybox stays on :latest deliberately — it is the minimal utility floor and the
// validated E2E image; language runtimes are pinned to a specific minor.
func Official() []Template {
	return []Template{
		{Name: "busybox", Image: "busybox:latest", Description: "Minimal shell utilities — smallest image, fastest to provision."},
		{Name: "python", Image: "python:3.12-slim", Description: "Python 3.12 runtime (slim)."},
		{Name: "node", Image: "node:20-slim", Description: "Node.js 20 runtime (slim)."},
		// M17 official starters — purpose-built envs an agent provisions in one call.
		{Name: "python-data", Image: "jupyter/scipy-notebook:latest", Description: "Python data stack (numpy, pandas, scipy, matplotlib) for data-analysis agents."},
		{Name: "browser", Image: "browserless/chrome:latest", Description: "Headless Chromium for browser/computer-use agents (M18)."},
	}
}

// Images returns the image refs of the official catalog (for the driver allow-list).
func Images() []string {
	cat := Official()
	out := make([]string, 0, len(cat))
	for _, t := range cat {
		out = append(out, t.Image)
	}
	return out
}
