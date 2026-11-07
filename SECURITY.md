# Security policy

hostping runs as root on RDMA hosts (see
[Privileges and devices](docs/operations.md#privileges-and-devices)): it opens
the RDMA and GPU device nodes, reads PCI config space and sysfs, forks
`nvidia-smi`, `ethtool` and an optional `--idle-gate-command`, can open uncore
PMUs with `perf_event_open`, serves `/metrics` and exchanges probe datagrams
with the host's own RNICs. Issues in those paths are taken seriously, for
example:

- a probe datagram, sysfs value, PCI config space or tool output that makes
  the daemon crash, hang, misreport a diagnosis or run something it should
  not;
- the metrics endpoint or the OTLP exporter leaking data or credentials, or
  ignoring `--metrics-web-config`;
- the packages or the systemd unit granting more than
  [docs/operations.md](docs/operations.md) says.

The metrics endpoint is plain HTTP without authentication unless
`--metrics-web-config` is set, as documented; that is not a vulnerability.

## Supported versions

hostping is pre-1.0. Security fixes are made on the `main` branch and
released in the next version. Only the latest release is supported.

## Reporting a vulnerability

Please do **not** open a public issue, pull request or discussion.

Report the vulnerability privately with GitHub's
[private vulnerability reporting](https://github.com/yuuki/hostping/security/advisories/new)
(the "Report a vulnerability" button of the Security tab). If that is not
possible, contact the author privately via the contact information on
https://github.com/yuuki.

Please include the hostping version (`hostping --version`), the distribution,
kernel, RNIC and driver, the command line or unit configuration, what an
attacker controls (a process on the host, the RDMA fabric, a file the daemon
reads, the network path to `/metrics` or the collector, ...), and the impact.
You should receive an answer within a few weeks; hostping is maintained on a
best-effort basis.
