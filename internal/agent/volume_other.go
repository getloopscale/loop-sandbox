//go:build !linux

package agent

import "errors"

var errVolumesNeedLinux = errors.New("volume mounts are supported only on linux")

func mountExt4(string, string) error { return errVolumesNeedLinux }

func unmountDir(string) error { return errVolumesNeedLinux }
