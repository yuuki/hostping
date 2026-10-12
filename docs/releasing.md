# Releasing hostping

A release is a `vX.Y.Z` tag and the `linux-amd64` tarball, `.deb` and `.rpm`
built from it, with a `SHA256SUMS` file, attached to the GitHub Release for
that tag. The release notes are the version's section of
[CHANGELOG.md](../CHANGELOG.md).

## What a release contains

`make docker-dist VERSION=X.Y.Z` writes four files to `dist/`:

```
dist/hostping-X.Y.Z-linux-amd64.tar.gz
dist/hostping_X.Y.Z_amd64.deb
dist/hostping-X.Y.Z-1.x86_64.rpm
dist/SHA256SUMS
```

The tarball has one top-level directory:

```
hostping-X.Y.Z/
  hostping
  hostpingctl
  libhostping_probe.so
  LICENSE  NOTICE  PATENTS.md  README.md
  docs/operations.md
  contrib/systemd/hostping.service
  contrib/systemd/hostping.default
```

Install it as [docs/operations.md](operations.md#install) describes: the three
binaries stay in one directory, because `hostping` and `hostpingctl` find
`libhostping_probe.so` through an `$ORIGIN` rpath.

The `.deb` and `.rpm` hold the same three binaries in `/usr/lib/hostping/`
(with `/usr/bin` symlinks; `$ORIGIN` resolves through them), the systemd unit
with `ExecStart` rewritten to that directory, `/etc/default/hostping` as a
config file, and the docs in `/usr/share/doc/hostping/`. They depend on
`libibverbs1 (>= 32)` / `libibverbs >= 32` and glibc 2.28. The package scripts
reload systemd; they never enable, start or restart the service, and stop and
disable it only on removal, not on an upgrade. [nfpm](https://nfpm.goreleaser.com/)
builds them from [packaging/nfpm/hostping.yaml](../packaging/nfpm/hostping.yaml)
(`make package`, part of `make dist`); the dev container pins its version.
deb and rpm need a version that starts with a digit, so `make dist` needs
`VERSION=X.Y.Z`.

The artifacts are built to be reproducible from the same commit and toolchain:
the Go binaries use `-trimpath` and `-buildvcs=false`, and the tarball uses
`tar --sort=name --mtime=@$SOURCE_DATE_EPOCH --owner=0 --group=0
--numeric-owner` (`SOURCE_DATE_EPOCH` defaults to the commit time) and
`gzip -n`. nfpm takes file mtimes from `SOURCE_DATE_EPOCH` too, and the `.rpm`
records the build host, which `make docker-dist` fixes with `--hostname`. Two
runs of `make docker-dist` on the same commit gave the same `SHA256SUMS`.

`make dist` needs GNU tar, nfpm and the amd64 `libibverbs` headers and stubs,
so run it in the dev container (`make docker-dist`), not on macOS.

## Runtime requirements on the target host

| Requirement | Why |
| --- | --- |
| Linux x86_64 with glibc **2.28** or newer | The build links against glibc 2.28 (see below). |
| `libibverbs.so.1` from rdma-core **32** or newer | The probe library needs the `IBVERBS_1.11` symbol version (`ibv_query_gid_ex`). Install it from the host's distribution (`libibverbs1` on Debian/Ubuntu, `libibverbs` on RHEL-family). |
| Optional: `libcuda.so.1` or `libcudart.so.*`, `libamdhip64.so` | Loaded with `dlopen` only when a GPU endpoint is probed. Without them the GPU endpoints are skipped with a reason; the binaries start and run without them. |

`hostping` and `hostpingctl` themselves need only `libc.so.6`,
`libresolv.so.2`, `libpthread.so.0`, and the bundled `libhostping_probe.so`; the
probe library adds `libibverbs.so.1`, `libdl.so.2`, and the dynamic loader.
The binaries' `RUNPATH` starts with `$ORIGIN`; it also carries the two
development paths from the cgo directive in `internal/zigprobe`
(`$ORIGIN/../zig/zig-out/lib` and the build container's
`/src/.../zig/zig-out/lib`), which do not exist on an installed host and come
after `$ORIGIN`.

Checked by running `hostping --version`, `hostpingctl version`, and
`hostpingctl diagnose --sim` from an extracted tarball in amd64 containers:

| Distribution | glibc | rdma-core | Result |
| --- | --- | --- | --- |
| Rocky Linux 8 | 2.28 | 48.0 | runs |
| Debian 11 | 2.31 | 33.2 | runs |
| Debian 12 | 2.36 | 44.0 | runs |
| Ubuntu 20.04 | 2.31 | 28.0 | fails: `IBVERBS_1.11` not found |

These runs exercise linking and the `--sim` path only, not an RNIC.

## glibc baseline

`zig cc` links against Zig's own glibc stubs for the version in the target
triple, so the oldest glibc a build runs on is a choice, not whatever the build
machine has. The Makefile pins it with `GLIBC_VERSION ?= 2.28` and uses
`x86_64-linux-gnu.2.28` for both the Zig probe library
(`zig build -Dtarget=...`) and the cgo link of the Go binaries
(`CC="zig cc -target ..."`).

Why 2.28: it is the glibc of the oldest enterprise distributions still in
support (RHEL, Rocky, and Alma Linux 8). Zig 0.17 also links the probe against
2.17, but the only common distribution that old (RHEL/CentOS 7) is out of
support, and rdma-core 32 (above) already rules out distributions older than
these. Raise `GLIBC_VERSION` only together with this page.

`make dist` runs `make check-glibc`, which fails if any of the three binaries
needs a `GLIBC_` symbol version newer than `GLIBC_VERSION`. With 2.28 the newest
required versions are:

| File | Newest `GLIBC_` | Newest `IBVERBS_` |
| --- | --- | --- |
| `libhostping_probe.so` | `GLIBC_2.28` | `IBVERBS_1.11` |
| `hostping` | `GLIBC_2.3.2` | none (through the probe library) |
| `hostpingctl` | `GLIBC_2.3.2` | none (through the probe library) |

To check by hand in the container: `objdump -T bin/linux-amd64/<file> | grep -o
'GLIBC_[0-9.]*' | sort -uV`.

## Cutting a release

1. Make sure `main` is green (`make docker-test`) and that the commit you want
   is on `main`.
2. On `main`, move the `[Unreleased]` entries of
   [CHANGELOG.md](../CHANGELOG.md) under a new `## [X.Y.Z] - YYYY-MM-DD`
   heading, update the version in the README's install example, and merge
   that. The workflow fails without a section for the tag's version.
3. Tag it and push the tag:

   ```bash
   git tag -a v1.2.3 -m v1.2.3
   git push origin v1.2.3
   ```

4. The [Release workflow](../.github/workflows/release.yml) runs on the tag: it
   runs `make docker-test` and `make docker-dist VERSION=1.2.3` on an arm64
   runner (the same cross-compile path as a local build on Apple Silicon); on
   amd64 it runs `hostping --version` and `hostpingctl diagnose --sim` from
   the extracted tarball, from the `.deb` installed on Ubuntu (plus
   `systemd-analyze verify` of the unit) and from the `.rpm` installed in a
   Rocky Linux 8 container, removing each package again; then it creates the
   GitHub Release with the CHANGELOG section as notes and the four files
   attached.

### Without GitHub Actions

When hosted Actions are not available, build and publish from a checkout of the
tag with Docker (colima or Docker Desktop):

```bash
git switch --detach v1.2.3
make docker-test
make docker-dist VERSION=1.2.3
(cd dist && sha256sum -c SHA256SUMS)        # shasum -a 256 -c SHA256SUMS on macOS
awk '/^## \[1\.2\.3\]/{f=1;next} f&&/^## /{exit} f' CHANGELOG.md > notes.md
gh release create v1.2.3 --verify-tag --notes-file notes.md --title v1.2.3 \
  dist/hostping-1.2.3-linux-amd64.tar.gz dist/hostping_1.2.3_amd64.deb \
  dist/hostping-1.2.3-1.x86_64.rpm dist/SHA256SUMS
```

Before publishing, run the extracted binaries once on an amd64 Linux host or
container with `libibverbs1` installed (`hostping --version`,
`hostpingctl diagnose --sim`), and install each package the same way
(`apt install ./dist/hostping_1.2.3_amd64.deb` in `debian:12`,
`dnf install ./dist/hostping-1.2.3-1.x86_64.rpm` in `rockylinux:8`).
`docker run --platform linux/amd64` works for this on Apple Silicon.

## Not shipped yet

- An apt or yum repository: the packages are attached to the GitHub Release
  only, and are not signed.
- `linux-arm64` artifacts: there is no arm64 RDMA hardware to test them on.
