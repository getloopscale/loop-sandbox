#!/usr/bin/env bash
# dev-k3d.sh — bring the M0 walking skeleton up on a local k3d cluster.
# Usage: scripts/dev-k3d.sh up | down | smoke
set -euo pipefail

CLUSTER="${CLUSTER:-loopsandbox}"
IMAGE="loopsandbox:dev"
CHART="deployments/kubernetes/loopsandbox"
RELEASE="lsb"

up() {
  if ! k3d cluster list | grep -q "^${CLUSTER}\b"; then
    k3d cluster create "${CLUSTER}" --wait
  fi
  docker build -t "${IMAGE}" .
  k3d image import "${IMAGE}" -c "${CLUSTER}"
  helm upgrade --install "${RELEASE}" "${CHART}" \
    --set image.repository=loopsandbox --set image.tag=dev
  kubectl rollout status deploy/${RELEASE}-postgres --timeout=120s
  kubectl rollout status deploy/${RELEASE}-api --timeout=120s
  echo "up: API service ${RELEASE}-api ready. Port-forward with:"
  echo "  kubectl port-forward svc/${RELEASE}-api 8080:80"
}

# smoke: end-to-end proof against the running cluster with real API-key auth
# (create tenant+key via sandboxctl, then create -> get -> destroy -> 404, + 401 without key).
smoke() {
  kubectl port-forward "svc/${RELEASE}-postgres" 15432:5432 >/tmp/lsb-pgpf.log 2>&1 &
  local pgpf=$!
  kubectl port-forward "svc/${RELEASE}-api" 18080:80 >/tmp/lsb-pf.log 2>&1 &
  pf=$!
  trap 'kill "${pf:-0}" "${pgpf:-0}" 2>/dev/null || true' EXIT
  sleep 4
  export DATABASE_URL="postgres://postgres:postgres@127.0.0.1:15432/loopsandbox?sslmode=disable"
  local tenant key base="http://127.0.0.1:18080"
  tenant=$(go run ./cmd/sandboxctl tenants create --name local | grep -o 'tnt_[a-f0-9-]*' | head -1)
  echo "tenant: ${tenant}"
  key=$(go run ./cmd/sandboxctl api-keys create --tenant "${tenant}" | grep -o 'lsb_[A-Za-z0-9_-]*' | head -1)
  echo "key: ${key:0:12}…"

  # 401 without a key
  local unauth
  unauth=$(curl -s -o /dev/null -w '%{http_code}' -XPOST "${base}/v1/sandboxes" -d '{}')
  [ "${unauth}" = "401" ] && echo "no-key: 401 OK" || { echo "FAIL: expected 401 got ${unauth}"; exit 1; }

  local id
  id=$(curl -sf -XPOST "${base}/v1/sandboxes" -H "Authorization: Bearer ${key}" \
        -d '{"image":"python:3"}' | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
  echo "created: ${id}"
  curl -sf "${base}/v1/sandboxes/${id}" -H "Authorization: Bearer ${key}" >/dev/null && echo "get: 200"
  curl -sf -XDELETE "${base}/v1/sandboxes/${id}" -H "Authorization: Bearer ${key}" -o /dev/null && echo "destroy: 204"
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "${base}/v1/sandboxes/${id}" -H "Authorization: Bearer ${key}")
  [ "${code}" = "404" ] && echo "get-after-destroy: 404 OK" || { echo "FAIL: expected 404 got ${code}"; exit 1; }
}

down() {
  k3d cluster delete "${CLUSTER}" || true
}

case "${1:-up}" in
  up) up ;;
  smoke) smoke ;;
  down) down ;;
  *) echo "usage: $0 up|smoke|down"; exit 2 ;;
esac
