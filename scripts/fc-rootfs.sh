#!/usr/bin/env bash
# Build a LoopSandbox Firecracker rootfs: inject the static lsb-agent as the guest
# init into a base ext4 image, root-free via debugfs (no loop-mount / sudo). The
# kernel boots with init=/usr/bin/lsb-agent (set by the driver's boot_args).
#
# Usage: scripts/fc-rootfs.sh [base.ext4] [out.ext4]
# Defaults to the assets under ~/.cache/loopsandbox/fc.
set -euo pipefail

FCDIR="${LOOPSANDBOX_FC_DIR:-$HOME/.cache/loopsandbox/fc}"
BASE="${1:-$FCDIR/rootfs.ext4}"
OUT="${2:-$FCDIR/rootfs-lsb.ext4}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEBUGFS="$(command -v debugfs || echo /usr/sbin/debugfs)"

[ -f "$BASE" ] || { echo "base rootfs not found: $BASE" >&2; exit 1; }

echo "==> build static lsb-agent (linux/amd64)"
# Build from the module root so `go` finds go.mod even when invoked with a different cwd
# (e.g. fc-assets.sh runs with cwd = the asset cache dir).
( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o "$FCDIR/lsb-agent" ./cmd/lsb-agent )

echo "==> clone base image → $OUT"
cp -f "$BASE" "$OUT"

echo "==> inject agent + workspace via debugfs"
"$DEBUGFS" -w "$OUT" >/dev/null 2>&1 <<EOF || true
rm /usr/bin/lsb-agent
EOF
"$DEBUGFS" -w "$OUT" >/dev/null <<EOF
write $FCDIR/lsb-agent /usr/bin/lsb-agent
set_inode_field /usr/bin/lsb-agent mode 0100755
mkdir /workspace
EOF

echo "==> verify"
if "$DEBUGFS" -R "stat /usr/bin/lsb-agent" "$OUT" 2>/dev/null | grep -q 'Mode:  0755'; then
  echo "    lsb-agent injected (mode 0755)"
else
  echo "    WARN: could not confirm agent mode" >&2
fi
echo "rootfs ready: $OUT"
