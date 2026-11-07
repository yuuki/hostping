# AGENTS.md

This file holds only what the code does not show: rules, contracts and
conventions. [docs/design.md](docs/design.md) is the source of truth for what
each live path does and where it fails closed; read it before changing
measurement, classification or diagnosis.

## Rules the code does not show

- English only in code, comments, docs, commit messages and PRs.
- Unknown is not zero: a reading that could not be taken is absent (no
  series, an `unknown` label, a skip reason in the report), never 0 or idle.
- Live paths fail closed with a reason; never fall back to the Fig. 2 topology
  or a stand-in measurement.
- Errors wrap their sentinel with `%w` and format the cause with `%v`
  (`"%w: %s: %v"`), so `errors.Is` sees the classification and not the cause's
  sentinels; that is why errorlint's errorf check is off.
- External commands: fixed argument vectors, no shell, a timeout and a
  `WaitDelay` on every one.
- The 1 s poll path stays cheap and forks rarely; log a persisting state once
  per change, not per poll.
- Series cardinality is bounded by the topology: no labels that grow with time
  or traffic; labels built from discovered strings go through `orUnknown`
  (`internal/telemetry`), which makes them valid UTF-8.
- Tests never need an RNIC, a GPU or root; pure Zig logic goes in files that
  `zig build test` covers without ibverbs (`zig/src/root_test.zig`).
- User-visible changes go under `[Unreleased]` in `CHANGELOG.md`.

## Compatibility contracts

Never change these silently; hostping is pre-1.0, so they may change between
minor versions, but every change is called out in the CHANGELOG.

- Metric names, units, labels and status codes: `internal/telemetry`,
  `docs/metrics.md`.
- Report JSON (fields, cause codes, evidence strings): `internal/model`.
- CLI flags of both binaries: `internal/config/config.go`.
- Baseline file format: `internal/baseline/parse.go`; `--state-dir` file:
  `internal/state`.
- C ABI (structs, error codes, error-name prefixes): `include/hostping.h` ↔
  `zig/src/abi.zig`, `zig/src/capi.zig` ↔ `internal/zigprobe`.
- UD mutual-probe tag (immediate data): `zig/src/udtag.zig` ↔ `seqMask` in
  `internal/mutual`.
- Alerting rules: `contrib/prometheus/hostping-rules.yml`.
- Packaging layout and systemd unit: `packaging/nfpm/`, `contrib/systemd/`.

## Before opening a pull request

Run `make check`. Also, when you:

- touch a parser or a fuzz target: `make test-fuzz FUZZTIME=30s`; commit any
  crasher under `testdata/fuzz/` with the fix.
- change a contract on purpose: regenerate the golden files with `-update`,
  review the diff, and update `docs/metrics.md`, `docs/design.md`,
  `docs/operations.md`, `docs/runbook.md` and the CHANGELOG.
- touch the Zig verbs code (`zig/src/verbs_linux.zig`) or a live probe path:
  run it on RDMA hardware (CI never does) and say in the PR what you ran and
  on what.
- touch packaging, the glibc baseline or the Dockerfile:
  `make docker-dist VERSION=X.Y.Z`.

## Git conventions

Branches: `<type>/<issue>-<summary>`, or `<type>/<summary>` without an issue
(`fix/166-connect-errors`, `feat/qp-counters`). `<type>` is a commit type
below; `<summary>` is 2–5 lowercase ASCII words joined by `-`. Branches named
by tools keep their names (`agent/*`, `claude/*`, `copilot/*`,
`dependabot/*`). Never push to the default branch.

Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

    <type>(<scope>)!: <subject>

    <body>

    <footer>

- `type`: `feat` (new user-visible behaviour), `fix` (bug fix), `perf`,
  `refactor` (no behaviour change), `test`, `docs`, `build` (build system,
  dependencies, packaging), `ci` (workflows), `chore` (anything else, e.g.
  `chore(release): prepare v1.2.3`), `revert`.
- `scope`: optional; the package or area changed: `analyzer`, `monitor`,
  `engine`, `telemetry`, `model`, `mutual`, `agent`, `zig`.
- `!` and a `BREAKING CHANGE: <what changed and how to migrate>` footer for
  any change to a compatibility contract above.
- `subject`: imperative, lower case (except identifiers), no final period,
  at most 72 characters including the prefix.
- `body`: why the change is needed and what it affects, wrapped at 72
  columns; required unless the subject says everything.
- `footer`: `Fixes #N` / `Refs #N`, `BREAKING CHANGE:`, trailers such as
  `Co-Authored-By:`.
- One logical change per commit; regenerated files go in the commit that
  changes their source. Pull request titles use the same format (they become
  the squash commit).

## Forbidden

- Disabling, skipping, deleting or weakening tests, golden files, fuzz
  properties or lint rules to get green; hiding failures (`|| true`,
  `continue-on-error`, blanket excludes). A `//nolint` needs the linter name
  and a reason.
- Editing generated or recorded outputs by hand (golden files: use `-update`;
  `zig-out/`).
- Adding dependencies without a justification in the PR (runtime ones need a
  strong one).
- Changing a compatibility contract without calling it out.
- Loosening workflow permissions, unpinning actions, adding
  `pull_request_target`, or adding caches to the release workflow.
- Committing secrets, rewriting history, force-pushing, tagging or publishing
  releases.

## Needs human review

Even with green CI: `.github/workflows/`, `packaging/` and `contrib/systemd/`
(privileges, hardening); anything that runs as root or forks (external
commands, the idle gate); parsers of sysfs, PCI config space and tool output;
the C ABI, the UD mutual-probe wire format and the Zig verbs code; the
compatibility contracts; dependency additions; large refactors.

## Environment notes

- With `GH_TOKEN` set but the GitHub API unreachable:
  `env -u GH_TOKEN make check`.
- The idle gate's process-group tests can flake on a heavily loaded machine;
  re-run them in isolation before concluding.
