//go:build !unix

package agent

import (
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// processes is a placeholder: background processes need a Unix guest.
type processes struct{}

func newProcesses() *processes { return &processes{} }

func (t *processes) killAll() {}

func (t *processes) logsAfter(int64, time.Duration) *agentproto.Response {
	return &agentproto.Response{Err: "sandbox logs are not supported on this platform"}
}

func (t *processes) running() int { return 0 }

func (h *Handler) processOp(*agentproto.Request) *agentproto.Response {
	return &agentproto.Response{Err: "background processes are not supported on this platform"}
}
