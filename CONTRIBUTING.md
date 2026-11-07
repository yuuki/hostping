# Contributing to hostping

Thank you for your interest. Bug reports, questions and pull requests are
welcome on GitHub. For a security issue, follow [SECURITY.md](SECURITY.md)
instead of opening an issue.

## Proposing a change

- Open an issue first for anything larger than a small fix, so that the
  approach can be agreed before you write it. hostping implements a published
  design ([docs/design.md](docs/design.md)); say how a change relates to it.
- Keep a pull request to one change, with tests for any change in behaviour,
  and the docs and the `[Unreleased]` section of [CHANGELOG.md](CHANGELOG.md)
  updated for anything a user would notice.
- Call out changes to metric names or labels, flags, the report JSON or the
  probe library's C ABI: they are contracts operators and dashboards rely on.
- Changes to live probe paths need a run on RDMA hardware; say in the pull
  request what you ran and on what.

## Checking your change

You need Go, Zig 0.17 and, on Linux, `libibverbs-dev`
([docs/building.md](docs/building.md); `make docker-test` runs the tests in
the dev container). Before opening a pull request:

```bash
make check     # golangci-lint, go vet, zig fmt, workflow lint, tests with -race
```

[AGENTS.md](AGENTS.md) lists the extra checks a change needs (fuzzing,
golden files, hardware runs).

## Commit messages

Commits, branches and pull request titles follow the Git conventions in
[AGENTS.md](AGENTS.md#git-conventions).

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE). See [PATENTS.md](PATENTS.md) for patent
information.
