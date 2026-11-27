#!/usr/bin/env bash
# Run the Firecracker provisioning benchmark (cold boot vs snapshot restore) on the
# host KVM. Reproducible M6 evidence. Requires /dev/kvm + assets (scripts/fc-assets.sh).
set -euo pipefail

FCDIR="${LOOPSANDBOX_FC_DIR:-$HOME/.cache/loopsandbox/fc}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

[ -r /dev/kvm ] || { echo "/dev/kvm not accessible — cannot benchmark microVMs" >&2; exit 1; }
[ -x "$FCDIR/firecracker" ] || { echo "assets missing — run scripts/fc-assets.sh" >&2; exit 1; }

export LOOPSANDBOX_FC_BIN="$FCDIR/firecracker"
export LOOPSANDBOX_FC_KERNEL="$FCDIR/vmlinux.bin"
export LOOPSANDBOX_FC_ROOTFS="$FCDIR/rootfs-lsb.ext4"

cd "$ROOT"
go test -tags=firecracker -run TestFirecracker_benchmark_provisioning -v -timeout 300s ./internal/runtime/firecracker/
