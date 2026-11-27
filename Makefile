CLUSTER ?= loopsandbox
CHART   := deployments/kubernetes/loopsandbox
IMAGE   := loopsandbox:dev

.PHONY: test load-test conformance test-integration lint vet helm-lint build ci dev dev-down sdk sdk-test sdk-e2e sbom fc-assets fc-rootfs fc-test fc-bench

test: ## unit + E2E (no docker)
	go test ./...

load-test: ## latency SLOs under concurrency (wall-clock; run on a quiet machine)
	go test -tags=load -count=1 -run TestLoad_ -v ./internal/app/

conformance: ## black-box conformance suite against a real driver (DRIVER=container|firecracker; needs docker)
	./scripts/conformance.sh $(or $(DRIVER),container)

test-integration: ## integration (real PostgreSQL via testcontainers — needs docker)
	go test -tags=integration -count=1 -timeout=180s ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

helm-lint:
	helm lint $(CHART)

build:
	CGO_ENABLED=0 go build -o bin/api ./cmd/api

ci: vet lint test helm-lint ## the gate the CI runs (mirrors M0 DoD #5)

dev: ## bring up k3d + PostgreSQL + API via Helm
	./scripts/dev-k3d.sh up

dev-down:
	./scripts/dev-k3d.sh down

sdk: ## build the TypeScript SDK
	cd sdk/javascript && npm install && npm run build

sdk-test: ## SDK unit tests (vitest)
	cd sdk/javascript && npm install && npm test

sdk-e2e: ## SDK end-to-end ship criterion (real PG + API + key; needs docker+node)
	./scripts/sdk-e2e.sh $(DRIVER)

sbom: ## generate CycloneDX SBOMs for the Go control plane + SDK
	./scripts/sbom.sh

fc-assets: ## fetch Firecracker binary + kernel + build the lsb-agent rootfs
	./scripts/fc-assets.sh

fc-rootfs: ## rebuild the lsb-agent rootfs from the base image
	./scripts/fc-rootfs.sh

fc-test: ## Firecracker driver integration tests (real microVMs; needs /dev/kvm + assets)
	LOOPSANDBOX_FC_BIN=$(HOME)/.cache/loopsandbox/fc/firecracker \
	LOOPSANDBOX_FC_KERNEL=$(HOME)/.cache/loopsandbox/fc/vmlinux.bin \
	LOOPSANDBOX_FC_ROOTFS=$(HOME)/.cache/loopsandbox/fc/rootfs-lsb.ext4 \
	go test -tags=firecracker -count=1 -timeout 20m ./internal/runtime/firecracker/

fc-bench: ## Firecracker provisioning benchmark (cold boot vs snapshot restore)
	./scripts/fc-bench.sh
