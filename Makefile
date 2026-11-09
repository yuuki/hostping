ZIG ?= zig
GO ?= go
PREFIX ?= .

# Tool versions. CI pins the same versions (.github/workflows); bump together.
GOLANGCI_LINT_VERSION   ?= v2.15.0
GOVULNCHECK_VERSION     ?= v1.9.0
ACTIONLINT_VERSION      ?= v1.7.12
ZIZMOR_VERSION          ?= 1.32.0
GH_ACTIONS_LOCK_VERSION ?= v0.4.1
FUZZTIME                ?= 30s
# Extra go test flags for each fuzz target, e.g. FUZZFLAGS=-parallel=2.
FUZZFLAGS               ?=
BENCH_COUNT             ?= 6

# go run builds a tool with the toolchain its own go.mod names, which can be
# older than this module's and then refuses to load it: build the tools with
# the toolchain this module selects (go.mod), as `go env GOVERSION` reports it.
GO_TOOLCHAIN = $(shell $(GO) env GOVERSION)
GOLANGCI_LINT ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
ACTIONLINT ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
GOVULNCHECK ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
# uvx or pipx (preinstalled on GitHub-hosted runners) runs the pinned zizmor.
ZIZMOR ?= $(if $(shell command -v uvx 2>/dev/null),uvx zizmor@$(ZIZMOR_VERSION),pipx run zizmor==$(ZIZMOR_VERSION))
# gh-actions-lock (the `gh actions-lock` extension) runs through `go run` too.
GH_ACTIONS_LOCK ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) $(GO) run github.com/github/gh-actions-lock/cmd/gh-actions-lock@$(GH_ACTIONS_LOCK_VERSION)

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
LIBEXT := dylib
else
LIBEXT := so
endif

# Stamped into both binaries (internal/buildinfo). Override for release builds:
#   make bin VERSION=1.2.3 COMMIT=abc1234
# docker-* targets resolve them on the host and pass them into the container,
# where git may not see the repository (a worktree, or a different owner).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDINFO_PKG := github.com/yuuki/hostping/internal/buildinfo
GO_LDFLAGS := -X $(BUILDINFO_PKG).Version=$(VERSION) -X $(BUILDINFO_PKG).Commit=$(COMMIT)

# Oldest glibc the linux-amd64 build links against: both the Zig probe library
# and the cgo link of the Go binaries resolve libc symbols from Zig's glibc
# stubs for this version, so the result runs on any glibc >= it. 2.28 is the
# glibc of the oldest supported enterprise distributions (RHEL/Rocky/Alma 8,
# Debian 10). docs/releasing.md records the symbol versions this produces.
GLIBC_VERSION ?= 2.28
ZIG_TARGET_AMD64 = x86_64-linux-gnu.$(GLIBC_VERSION)
GO_BUILDFLAGS_AMD64 := -trimpath

# Release tarball (make dist / make docker-dist). A leading "v" is dropped so
# VERSION=v1.2.3 and VERSION=1.2.3 name the same artifact.
DIST_VERSION = $(patsubst v%,%,$(VERSION))
DIST_NAME = hostping-$(DIST_VERSION)
DIST_TARBALL = $(DIST_NAME)-linux-amd64.tar.gz
# File mtimes inside the tarball: the commit time, so a rebuild of the same
# commit gives the same archive. Resolved on the host like VERSION.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
DIST_DOCS := LICENSE NOTICE PATENTS.md README.md

# .deb / .rpm (make package, part of make dist). packaging/nfpm/hostping.yaml
# installs the binaries in /usr/lib/hostping/, so the packaged unit runs them
# from there instead of /opt/hostping/current.
NFPM ?= nfpm
NFPM_ARCH := amd64
PACKAGE_UNIT := bin/linux-amd64/hostping.service

DOCKER ?= docker
IMAGE ?= hostping-dev
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),arm64)
DOCKER_PLATFORM ?= linux/arm64
else ifeq ($(UNAME_M),aarch64)
DOCKER_PLATFORM ?= linux/arm64
else
DOCKER_PLATFORM ?= linux/amd64
endif
# --hostname: the .rpm records the build host, so a random container name
# would make every rebuild of the same commit differ.
DOCKER_RUN = $(DOCKER) run --rm --platform $(DOCKER_PLATFORM) \
	--hostname hostping-build \
	-e GOCACHE=/tmp/go-cache \
	-e GOMODCACHE=/tmp/gomod-cache \
	-e ZIG_LOCAL_CACHE_DIR=/tmp/zig-local \
	-e ZIG_GLOBAL_CACHE_DIR=/tmp/zig-global \
	-e VERSION="$(VERSION)" \
	-e COMMIT="$(COMMIT)" \
	-e SOURCE_DATE_EPOCH="$(SOURCE_DATE_EPOCH)" \
	-e GLIBC_VERSION="$(GLIBC_VERSION)" \
	-e GOFLAGS=-buildvcs=false \
	-v "$(CURDIR)":/src \
	-w /src \
	$(IMAGE)

.PHONY: all zig test test-race test-fuzz bench check lint fmt lint-actions lock-actions vuln bin bin-linux-amd64 package dist release check-glibc tidy complexity setup docker-image docker-test docker-bin docker-bin-amd64 docker-dist docker-setup docker-shell

all: test bin

# Native host toolchain (mise). On macOS this is --sim only.
setup:
	mise install
	$(MAKE) test bin

# Linux container with libibverbs. Preferred on macOS.
docker-setup: docker-image docker-test docker-bin

docker-image:
	$(DOCKER) build --platform $(DOCKER_PLATFORM) -t $(IMAGE) .

docker-test: docker-image
	$(DOCKER_RUN) make test

docker-bin: docker-image
	$(DOCKER_RUN) make bin

# ELF x86_64 for real RDMA hosts. Cross-compiles from the arm64 image
# (Zig under Rosetta linux/amd64 fails on translate-c).
docker-bin-amd64: docker-image
	$(DOCKER_RUN) make bin-linux-amd64

# Release tarball, .deb, .rpm and SHA256SUMS in dist/. Run in the container: it
# needs GNU tar, nfpm and the amd64 libibverbs, e.g. make docker-dist VERSION=1.2.3
docker-dist: docker-image
	$(DOCKER_RUN) make dist

docker-shell: docker-image
	$(DOCKER) run --rm -it --platform $(DOCKER_PLATFORM) \
		-e GOCACHE=/tmp/go-cache \
		-e GOMODCACHE=/tmp/gomod-cache \
		-e ZIG_LOCAL_CACHE_DIR=/tmp/zig-local \
		-e ZIG_GLOBAL_CACHE_DIR=/tmp/zig-global \
		-v "$(CURDIR)":/src \
		-w /src \
		$(IMAGE) bash

zig:
	cd zig && $(ZIG) build -Doptimize=ReleaseSafe
ifeq ($(UNAME_S),Darwin)
	install_name_tool -id '@rpath/libhostping_probe.dylib' zig/zig-out/lib/libhostping_probe.dylib
endif

test: zig
	cd zig && $(ZIG) build test
	CGO_ENABLED=1 $(GO) test ./...

# make test with the race detector on the Go tests.
test-race: zig
	cd zig && $(ZIG) build test
	CGO_ENABLED=1 $(GO) test -race ./...

# Every Go fuzz target for FUZZTIME each (make test-fuzz FUZZTIME=3m). The
# targets live in pure-Go packages, so this needs no probe library; plain
# make test runs their seed corpora.
test-fuzz:
	@set -e; for dir in $$(grep -rl --include='*_test.go' '^func Fuzz' . | xargs -n1 dirname | sort -u); do \
		for f in $$($(GO) test -list '^Fuzz' $$dir | grep '^Fuzz'); do \
			echo "== $$dir $$f"; \
			$(GO) test -run='^$$' -fuzz="^$$f\$$" -fuzztime=$(FUZZTIME) $(FUZZFLAGS) $$dir; \
		done; \
	done

# Benchmarks in benchstat format, also written to bench.txt.
bench: zig
	CGO_ENABLED=1 $(GO) test -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT) ./... | tee bench.txt

# The local pre-PR gate: what the required CI jobs check, without root, RDMA
# hardware or Docker.
check: lint lint-actions test-race

# golangci-lint (linters and gofmt, .golangci.yml), go vet, go.mod tidiness
# and zig fmt. The probe library is built first: cgo links internal/zigprobe
# and the commands even to type-check them.
lint: zig
	$(GOLANGCI_LINT) run
	CGO_ENABLED=1 $(GO) vet ./...
	$(GO) mod tidy -diff
	cd zig && $(ZIG) fmt --check src

# Apply the formatting make lint checks.
fmt:
	$(GOLANGCI_LINT) fmt
	cd zig && $(ZIG) fmt src

# Lint (actionlint) and audit (zizmor) the GitHub Actions workflows. Without
# GH_TOKEN zizmor runs its offline audits only; CI sets it.
# gh-actions-lock checks that .github/workflows/actions.lock pins every
# action the workflows use; with GH_TOKEN it also verifies each pin upstream.
lint-actions:
	$(ACTIONLINT)
	$(ZIZMOR) $(if $(GH_TOKEN),,--offline) .
	$(GH_ACTIONS_LOCK) $(if $(GH_TOKEN),--verify,--verify-local)

# Pin the actions in .github/workflows/actions.lock after a `uses:` changed,
# then review the diff. Needs a GitHub token (GH_TOKEN or `gh auth login`).
lock-actions:
	$(GH_ACTIONS_LOCK)

# Known vulnerabilities reachable from the code.
vuln: zig
	$(GOVULNCHECK) ./...

bin: zig
	mkdir -p bin
	cp -f zig/zig-out/lib/libhostping_probe.$(LIBEXT) bin/
	CGO_ENABLED=1 $(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/hostping ./cmd/hostping
	CGO_ENABLED=1 $(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/hostpingctl ./cmd/hostpingctl
ifeq ($(UNAME_S),Darwin)
	-install_name_tool -add_rpath '@loader_path' bin/hostping
	-install_name_tool -add_rpath '@loader_path' bin/hostpingctl
endif

# Linux amd64 + libibverbs. Intended to run inside the dev container.
bin-linux-amd64:
	cd zig && $(ZIG) build -Doptimize=ReleaseSafe -Dtarget=$(ZIG_TARGET_AMD64)
	mkdir -p bin/linux-amd64
	cp -f zig/zig-out/lib/libhostping_probe.so bin/linux-amd64/
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		CC="$(ZIG) cc -target $(ZIG_TARGET_AMD64)" \
		CGO_LDFLAGS="-L/usr/lib/x86_64-linux-gnu" \
		$(GO) build $(GO_BUILDFLAGS_AMD64) -ldflags "$(GO_LDFLAGS)" -o bin/linux-amd64/hostping ./cmd/hostping
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		CC="$(ZIG) cc -target $(ZIG_TARGET_AMD64)" \
		CGO_LDFLAGS="-L/usr/lib/x86_64-linux-gnu" \
		$(GO) build $(GO_BUILDFLAGS_AMD64) -ldflags "$(GO_LDFLAGS)" -o bin/linux-amd64/hostpingctl ./cmd/hostpingctl

# Fails if any linux-amd64 artifact needs a GLIBC_ symbol version newer than
# GLIBC_VERSION. Needs binutils (objdump), as in the dev container.
check-glibc:
	@max=$$(for f in bin/linux-amd64/hostping bin/linux-amd64/hostpingctl bin/linux-amd64/libhostping_probe.so; do \
		objdump -T "$$f" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' ; done | sed 's/^GLIBC_//' | sort -u -V | tail -n 1); \
	if [ -z "$$max" ]; then echo "check-glibc: no GLIBC_ symbols found" >&2; exit 1; fi; \
	if [ "$$(printf '%s\n%s\n' "$$max" "$(GLIBC_VERSION)" | sort -V | tail -n 1)" != "$(GLIBC_VERSION)" ]; then \
		echo "check-glibc: needs GLIBC_$$max, newer than the GLIBC_VERSION=$(GLIBC_VERSION) baseline" >&2; exit 1; fi; \
	echo "check-glibc: newest required symbol version GLIBC_$$max (baseline $(GLIBC_VERSION))"

# dist/hostping_<version>_amd64.deb and dist/hostping-<version>-1.x86_64.rpm.
# Needs nfpm (in the dev container): use make docker-dist. deb and rpm need a
# version that starts with a digit, so pass VERSION=1.2.3.
package: bin-linux-amd64 check-glibc
	@case "$(DIST_VERSION)" in [0-9]*) ;; *) echo "package: VERSION '$(VERSION)' must start with a digit, e.g. VERSION=1.2.3" >&2; exit 1 ;; esac
	sed 's#^ExecStart=/opt/hostping/current/hostping #ExecStart=/usr/lib/hostping/hostping #' \
		contrib/systemd/hostping.service > $(PACKAGE_UNIT)
	@grep -q '^ExecStart=/usr/lib/hostping/hostping ' $(PACKAGE_UNIT) || \
		{ echo "package: ExecStart in contrib/systemd/hostping.service was not rewritten" >&2; exit 1; }
	mkdir -p dist
	for p in deb rpm; do \
		NFPM_VERSION="$(DIST_VERSION)" NFPM_ARCH=$(NFPM_ARCH) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
			$(NFPM) package --config packaging/nfpm/hostping.yaml --packager $$p --target dist/ || exit 1; \
	done

# dist/hostping-<version>-linux-amd64.tar.gz, the .deb and .rpm (make package)
# and dist/SHA256SUMS over all three. Needs GNU tar (--sort, --mtime) and nfpm:
# use make docker-dist on macOS.
dist: package
	@case "$(DIST_VERSION)" in ""|*/*|*" "*) echo "dist: bad VERSION '$(VERSION)'" >&2; exit 1 ;; esac
	@# Staged outside the source tree: tar on a Docker bind mount can report
	@# "file changed as we read it" for files written moments earlier.
	set -e; stage=$$(mktemp -d); trap 'rm -rf "$$stage"' EXIT; \
	d="$$stage/$(DIST_NAME)"; \
	mkdir -p "$$d/docs" "$$d/contrib/systemd"; \
	install -m 0755 bin/linux-amd64/hostping bin/linux-amd64/hostpingctl "$$d/"; \
	install -m 0644 bin/linux-amd64/libhostping_probe.so $(DIST_DOCS) "$$d/"; \
	install -m 0644 docs/operations.md docs/design.md docs/metrics.md docs/runbook.md "$$d/docs/"; \
	install -m 0644 contrib/systemd/* "$$d/contrib/systemd/"; \
	tar --format=gnu --sort=name --mtime=@$(SOURCE_DATE_EPOCH) \
		--owner=0 --group=0 --numeric-owner --mode=u+w,go-w,a+rX \
		-C "$$stage" -cf "$$stage/dist.tar" $(DIST_NAME); \
	mkdir -p dist; \
	gzip -n -9 -c "$$stage/dist.tar" > dist/$(DIST_TARBALL)
	cd dist && sha256sum $(DIST_TARBALL) hostping_$(DIST_VERSION)_$(NFPM_ARCH).deb \
		hostping-$(DIST_VERSION)-1.x86_64.rpm > SHA256SUMS
	@cat dist/SHA256SUMS

# Alias: make release VERSION=1.2.3
release: dist

tidy:
	$(GO) mod tidy

# Rank the most complex functions with cccc (https://github.com/moznion/cccc);
# what is analyzed is configured in cccc.toml.
complexity:
	cccc --table --top-cognitive 20 .
