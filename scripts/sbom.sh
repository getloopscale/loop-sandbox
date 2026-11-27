#!/usr/bin/env bash
# Generate CycloneDX SBOMs for the Go control plane and the TypeScript SDK (M5 ship
# requirement). Uses spec-compliant generators — no hand-rolled SBOM (Rule 9).
#
#   Go  : cyclonedx-gomod (go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest)
#   SDK : npm sbom --sbom-format cyclonedx (npm >= 10)
#
# Outputs sbom/loopsandbox-go.cdx.json and sbom/loopsandbox-sdk.cdx.json.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
mkdir -p sbom

GOMOD_BIN="$(go env GOPATH)/bin/cyclonedx-gomod"
if [ ! -x "$GOMOD_BIN" ]; then
  echo "cyclonedx-gomod not found; run: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest" >&2
  exit 1
fi

echo "==> Go SBOM (CycloneDX)"
"$GOMOD_BIN" mod -json -licenses -output sbom/loopsandbox-go.cdx.json .

echo "==> SDK SBOM (CycloneDX)"
( cd sdk/javascript && npm sbom --sbom-format cyclonedx > "$ROOT/sbom/loopsandbox-sdk.cdx.json" )

echo "SBOMs written to sbom/"
