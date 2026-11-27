package firecracker

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// fileOp sends one file operation to the guest agent and maps its error codes.
func (d *Driver) fileOp(id runtime.SandboxID, req *agentproto.Request) (*agentproto.Response, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	resp, err := d.roundTrip(v, req, d.cfg.FileOpTimeout)
	if err != nil {
		return nil, err
	}
	switch resp.Err {
	case "":
		return resp, nil
	case agentproto.ErrNotFound:
		return nil, runtime.ErrFileNotFound
	case agentproto.ErrExists:
		return nil, runtime.ErrFileExists
	default:
		return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
}

// StatFile implements runtime.FileOpsDriver.
func (d *Driver) StatFile(_ context.Context, id runtime.SandboxID, filePath string) (*runtime.FileInfo, error) {
	resp, err := d.fileOp(id, &agentproto.Request{Op: agentproto.OpStat, Path: filePath})
	if err != nil {
		return nil, err
	}
	if resp.Info == nil {
		return nil, fmt.Errorf("stat %q: the guest agent answered without file info; rebuild the rootfs with make fc-rootfs", filePath)
	}
	return &runtime.FileInfo{
		Name: path.Base(filePath), Size: resp.Info.Size, IsDir: resp.Info.IsDir,
		Mode: resp.Info.Mode, ModTime: time.Unix(resp.Info.ModTime, 0).UTC(),
	}, nil
}

// MakeDir implements runtime.FileOpsDriver.
func (d *Driver) MakeDir(_ context.Context, id runtime.SandboxID, dirPath string) error {
	_, err := d.fileOp(id, &agentproto.Request{Op: agentproto.OpMkdir, Path: dirPath})
	return err
}

// MoveFile implements runtime.FileOpsDriver.
func (d *Driver) MoveFile(_ context.Context, id runtime.SandboxID, from, to string, overwrite bool) error {
	_, err := d.fileOp(id, &agentproto.Request{Op: agentproto.OpMove, Path: from, To: to, Overwrite: overwrite})
	return err
}

var _ runtime.FileOpsDriver = (*Driver)(nil)
