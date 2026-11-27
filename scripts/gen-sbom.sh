#!/usr/bin/env bash
# Regenerate the CycloneDX SBOM for the Go module. Deterministic — no network beyond
# the module graph the build already resolved. Requires cyclonedx-gomod:
#   go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest
set -euo pipefail
cd "$(dirname "$0")/.."
out="docs/supply-chain/sbom.cdx.json"
cyclonedx-gomod mod -json -output "$out"
echo "wrote $out ($(python3 -c "import json;print(len(json.load(open('$out')).get('components',[])))") components)"
