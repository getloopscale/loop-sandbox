//go:build !linux

package agent

import "github.com/getloopscale/loop-sandbox/internal/agentproto"

// watches is a placeholder: directory watches need inotify.
type watches struct{}

func newWatches() *watches { return &watches{} }

func (t *watches) closeAll() {}

func (h *Handler) watchOp(*agentproto.Request) *agentproto.Response {
	return &agentproto.Response{Err: "directory watches are not supported on this platform"}
}
