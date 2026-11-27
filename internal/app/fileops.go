package app

import (
	"context"
	"errors"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// ErrFileOpsUnsupported is returned when the runtime cannot stat, create
// directories or move paths.
var ErrFileOpsUnsupported = errors.New("file operations not supported by this runtime")

// workspaceRoot is the directory every sandbox path lives under.
const workspaceRoot = "/workspace"

// fileOps resolves the actor's sandbox and the driver capability shared by the
// file operations below.
func (s *Service) fileOps(ctx context.Context, actor Actor, sandboxID string) (runtime.FileOpsDriver, error) {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	fd, ok := s.driver.(runtime.FileOpsDriver)
	if !ok || !s.supports(runtime.CapFileOps) {
		return nil, ErrFileOpsUnsupported
	}
	return fd, nil
}

// StatFile describes a path in the actor's sandbox.
func (s *Service) StatFile(ctx context.Context, actor Actor, sandboxID, filePath string) (*runtime.FileInfo, error) {
	fd, err := s.fileOps(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	clean, err := domain.ValidateSandboxPath(filePath)
	if err != nil {
		return nil, err
	}
	info, err := fd.StatFile(ctx, runtime.SandboxID(sandboxID), clean)
	if err != nil {
		return nil, s.mapRuntimeErr(err)
	}
	return info, nil
}

// MakeDir creates a directory, with its parents, in the actor's sandbox. A
// directory that already exists is not an error.
func (s *Service) MakeDir(ctx context.Context, actor Actor, sandboxID, dirPath string) error {
	fd, err := s.fileOps(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	clean, err := domain.ValidateSandboxPath(dirPath)
	if err != nil {
		return err
	}
	if err := fd.MakeDir(ctx, runtime.SandboxID(sandboxID), clean); err != nil {
		return s.mapRuntimeErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.file.mkdir", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}

// MoveFile renames a path in the actor's sandbox. An existing destination is
// replaced only when overwrite is set.
func (s *Service) MoveFile(ctx context.Context, actor Actor, sandboxID, from, to string, overwrite bool) error {
	fd, err := s.fileOps(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	cleanFrom, err := domain.ValidateSandboxPath(from)
	if err != nil {
		return &domain.ValidationError{Field: "from", Reason: "must be a path under /workspace"}
	}
	cleanTo, err := domain.ValidateSandboxPath(to)
	if err != nil {
		return &domain.ValidationError{Field: "to", Reason: "must be a path under /workspace"}
	}
	switch {
	case cleanFrom == workspaceRoot:
		return &domain.ValidationError{Field: "from", Reason: "the workspace root cannot be moved"}
	case cleanTo == workspaceRoot:
		return &domain.ValidationError{Field: "to", Reason: "the workspace root cannot be replaced"}
	case cleanFrom == cleanTo:
		return &domain.ValidationError{Field: "to", Reason: "is the same path as from"}
	case len(cleanTo) > len(cleanFrom) && cleanTo[:len(cleanFrom)+1] == cleanFrom+"/":
		return &domain.ValidationError{Field: "to", Reason: "is inside from"}
	}
	if err := fd.MoveFile(ctx, runtime.SandboxID(sandboxID), cleanFrom, cleanTo, overwrite); err != nil {
		return s.mapRuntimeErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.file.move", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}
