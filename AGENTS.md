# AGENTS.md

## Rules
- Unknown is not zero: a reading that could not be taken is absent (no series, an `unknown` label, a skip reason in the report), never 0 or idle.
- Live paths fail closed with a reason, never fall back to the Fig. 2 topology or a stand-in measurement; read `docs/design.md` before changing them.
- Errors wrap only their sentinel with `%w` and format the cause with `%v` (`"%w: %s: %v"`), so `errors.Is` sees the classification, not the cause.
- Label values built from discovered strings (sysfs device and netdev names) go through `orUnknown` in `internal/telemetry`.
- Tests never need an RNIC, a GPU or root; pure Zig logic goes in files that `zig build test` covers without ibverbs (`zig/src/root_test.zig`).
- The idle gate's process-group tests can flake on a heavily loaded machine; re-run them in isolation before concluding.

## Compatibility contracts
- Metric names, units, labels and status codes: `internal/telemetry`, `docs/metrics.md`.
- Report JSON (fields, cause codes, evidence strings): `internal/model`; CLI flags of both binaries: `internal/config/config.go`.
- Baseline file format (`internal/baseline/parse.go`); `--state-dir` file (`internal/state`); alerting rules (`contrib/prometheus/hostping-rules.yml`).
- C ABI (structs, error codes, error-name prefixes): `include/hostping.h` ↔ `zig/src/abi.zig`, `zig/src/capi.zig` ↔ `internal/zigprobe`.
- UD mutual-probe tag (immediate data): `zig/src/udtag.zig` ↔ `seqMask` in `internal/mutual`.
- Packaging layout and systemd unit: `packaging/nfpm/`, `contrib/systemd/`.

## Before opening a pull request
- `make check`; also parsers or fuzz targets → `make test-fuzz FUZZTIME=30s`; packaging, glibc baseline or Dockerfile → `make docker-dist VERSION=X.Y.Z`.
- A contract changed on purpose → regenerate its golden files with `-update`, review the diff, update `docs/` and the `CHANGELOG.md`.
- `zig/src/verbs_linux.zig` or a live probe path → run it on RDMA hardware (CI never does) and state in the PR what ran where.

## Git conventions
- Branches: `<type>/<issue>-<summary>` (`<type>/<summary>` without an issue); tools' `agent/*`, `claude/*`, `copilot/*`, `dependabot/*` keep their names.
- Commits and PR titles: Conventional Commits 1.0.0, types `feat` `fix` `perf` `refactor` `test` `docs` `build` `ci` `chore` `revert`; scopes: `analyzer`, `monitor`, `engine`, `telemetry`, `model`, `mutual`, `agent`, `zig`.
