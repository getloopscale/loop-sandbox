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
		// Built by this repository (templates/Dockerfile, published by the templates
		// workflow). The names are the ones of the e2b templates, so a script that
		// creates a sandbox from one of them keeps its argument.
		{Name: "base", Image: built("base"), Description: "Debian with Node.js 22, Python 3, pip, git, curl, jq and ripgrep."},
		{Name: "claude", Image: built("claude"), Description: "The base template with the Claude Code CLI (claude)."},
		{Name: "codex", Image: built("codex"), Description: "The base template with the OpenAI Codex CLI (codex)."},
		{Name: "gemini", Image: built("gemini"), Description: "The base template with the Gemini CLI (gemini)."},
		{Name: "opencode", Image: built("opencode"), Description: "The base template with the OpenCode CLI (opencode)."},
		{Name: "openclaw", Image: built("openclaw"), Description: "The base template with the OpenClaw CLI (openclaw)."},
		{Name: "droid", Image: built("droid"), Description: "The base template with the Factory CLI (droid)."},
		{Name: "amp", Image: built("amp"), Description: "The base template with the Amp CLI (amp)."},
		{Name: "desktop", Image: built("desktop"), Description: "The base template with an Xfce desktop on a virtual display: run start-desktop, then scrot and xdotool."},
	}
}

// builtVersion is the TEMPLATES_VERSION of .github/workflows/templates.yml: the
// version of the images this repository builds. A published tag is never
// overwritten, so a change to templates/ raises both.
const builtVersion = "2026.11.28"

// built names the image of a template this repository builds.
func built(name string) string {
	return "ghcr.io/getloopscale/loop-sandbox:template-" + name + "-" + builtVersion
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
