# disco-vm builds with plain `go build` everywhere. On macOS the binary also
# has to be signed with the virtualization entitlement before it can create a
# VM, and a rebuild drops the signature, so build through here.

BIN ?= bin/disco-vm
ENTITLEMENTS := cmd/disco-vm/disco-vm.entitlements
UNAME := $(shell uname -s)

.PHONY: build vet test test-vz test-e2e-vz

build:
	go build -o $(BIN) ./cmd/disco-vm
ifeq ($(UNAME),Darwin)
	codesign --force --sign - --entitlements $(ENTITLEMENTS) $(BIN)
endif

vet:
	go vet ./... && GOOS=darwin go vet ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./...

test:
	go test ./...

# The vz conformance suite on real hardware. DISCO_VM_VZ_BASE names an
# installed layer's directory (see docs/drivers/vz.md); without it the suite
# skips.
# caffeinate -d keeps the display awake, so the Mac does not lock mid-run: a
# vz restore needs the screen unlocked.
test-vz:
	caffeinate -d go test -exec $(CURDIR)/scripts/codesign-exec -timeout 4h -v -run 'TestConformance|TestResumeOrFallBack|TestWarmUser|TestTemplates' ./pkg/machine/vz

# internal/e2e against vz: the signed binary, and a state root that keeps its
# base install between runs (the first run installs macOS).
DISCO_VM_E2E_ROOT ?= $(HOME)/Library/Application Support/disco-vm-e2e

test-e2e-vz: build
	DISCO_VM_DRIVER=vz DISCO_VM_TEST_BINARY=$(CURDIR)/$(BIN) DISCO_VM_E2E_ROOT="$(DISCO_VM_E2E_ROOT)" \
		caffeinate -d go test -timeout 4h -v ./internal/e2e
