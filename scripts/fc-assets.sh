#!/usr/bin/env bash
# Fetch the Firecracker assets the microVM driver needs (binary + guest kernel +
# base rootfs) into ~/.cache/loopsandbox/fc, then build the lsb-agent rootfs.
# Reproducible: pinned versions. Requires: curl, tar, go, debugfs.
set -euo pipefail

FCVER="v1.10.1"
FCDIR="${LOOPSANDBOX_FC_DIR:-$HOME/.cache/loopsandbox/fc}"
ARCH="$(uname -m)"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$FCDIR"
cd "$FCDIR"

echo "==> firecracker + jailer ${FCVER}"
if [ ! -x "$FCDIR/firecracker" ]; then
  curl -sSL -o fc.tgz "https://github.com/firecracker-microvm/firecracker/releases/download/${FCVER}/firecracker-${FCVER}-${ARCH}.tgz"
  tar xzf fc.tgz
  cp "release-${FCVER}-${ARCH}/firecracker-${FCVER}-${ARCH}" firecracker && chmod +x firecracker
  cp "release-${FCVER}-${ARCH}/jailer-${FCVER}-${ARCH}" jailer && chmod +x jailer
  rm -rf fc.tgz "release-${FCVER}-${ARCH}"
fi

echo "==> guest kernel (vmlinux 5.10)"
[ -f vmlinux.bin ] || curl -sSL -o vmlinux.bin "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}/vmlinux-5.10.223"

echo "==> base rootfs (Ubuntu 22.04)"
[ -f rootfs.ext4 ] || curl -sSL -o rootfs.ext4 "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}/ubuntu-22.04.ext4"

echo "==> build lsb-agent rootfs"
bash "$ROOT/scripts/fc-rootfs.sh"

cat <<ENV

Assets ready in $FCDIR. Export for the driver / benchmark:
  export LOOPSANDBOX_FC_BIN=$FCDIR/firecracker
  export LOOPSANDBOX_FC_KERNEL=$FCDIR/vmlinux.bin
  export LOOPSANDBOX_FC_ROOTFS=$FCDIR/rootfs-lsb.ext4
ENV
