# Building hostping

Needs Go 1.27, Zig 0.17 (`mise.toml`). Live probes also need `libibverbs` (Linux). `make bin` copies the probe library next to the binaries (`$ORIGIN` / `@loader_path`).

The **Linux container** is the default development environment. It links `libibverbs` the same way a real RDMA host does. `--sim` still needs no NIC.

```bash
# macOS: start the Linux VM if needed
colima start               # or Docker Desktop
make docker-setup          # image + test + bin
make docker-test
make docker-bin
make docker-bin-amd64      # ELF x86_64 for real RDMA hosts → bin/linux-amd64/
make docker-dist VERSION=1.2.3   # release tarball, .deb, .rpm + SHA256SUMS → dist/
make docker-shell          # interactive Linux toolchain
```

On Apple Silicon, `make docker-*` forces `--platform linux/arm64`. That overrides a host `DOCKER_DEFAULT_PLATFORM=linux/amd64`, which would otherwise run Zig under Rosetta and fail.

Cursor / VS Code can also **Reopen in Container** via `.devcontainer/`.

`make bin` and `make bin-linux-amd64` (and so `make docker-bin*`) stamp both binaries with `VERSION` (`git describe --tags --always --dirty`) and `COMMIT` (`git rev-parse --short HEAD`). Override them for a release build, e.g. `make docker-bin-amd64 VERSION=1.2.3 COMMIT=abc1234`. A plain `go build` falls back to the module version and VCS stamping, and to `devel` / `unknown` without them. `hostping --version` and `hostpingctl version` print the result:

```
hostping 1.2.3 (commit abc1234, zig probe 0.2.0, go1.27.0)
```

`make docker-bin` writes Linux ELF for the container arch into `bin/` (and a `.so` into `zig/zig-out`). `make docker-bin-amd64` writes `x86_64` binaries into `bin/linux-amd64/` (copy that directory onto the host; `libibverbs` comes from the target OS). Rebuild with native `make bin` if you need Darwin binaries again.

The `x86_64` build links against glibc 2.28 (`GLIBC_VERSION` in the Makefile), so it runs on glibc 2.28 or newer; the target host also needs `libibverbs.so.1` from rdma-core 32 or newer. CUDA and HIP libraries are loaded with `dlopen` only when present.

`make docker-dist VERSION=X.Y.Z` builds the release tarball, `.deb`, `.rpm` and `SHA256SUMS` locally; [releasing.md](releasing.md) covers the artifact layout and how to cut a release.

## Native macOS (optional, `--sim` only)

Apple Silicon can build and run the Fig.2 path without Docker. Live sysfs / ibverbs stay Linux-only and fail closed.

1. Xcode Command Line Tools: `xcode-select --install`
2. [mise](https://mise.jdx.dev/): `brew install mise` (and `mise activate` in your shell)
3. `make setup`

Zig 0.14 and 0.15 do not link the build runner on current Apple Silicon macOS. Use the pinned Go 1.27.2 and Zig 0.17.0 from `mise.toml`.
