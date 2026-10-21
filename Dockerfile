FROM golang:1.27.3-bookworm

ARG TARGETARCH
# Bump these three together; checksums come from https://ziglang.org/download/index.json
ENV ZIG_VERSION=0.17.0 \
    ZIG_SHA256_X86_64=1cbe9df9f27e6b78d14ccbca43b6703a404ef79ef1c463de901d7f088d4e2026 \
    ZIG_SHA256_AARCH64=9e8d11661d4ae3bd57702a3832781e23ad151dde5798e16a5ccd503f65234ff8

RUN set -eux; \
    if [ "${TARGETARCH}" = "arm64" ]; then dpkg --add-architecture amd64; fi; \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        libibverbs-dev \
        pkg-config \
        xz-utils \
        $([ "${TARGETARCH}" = "arm64" ] && echo libibverbs-dev:amd64); \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) zig_arch=x86_64; zig_sha256="${ZIG_SHA256_X86_64}" ;; \
      arm64) zig_arch=aarch64; zig_sha256="${ZIG_SHA256_AARCH64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    zig_tarball="zig-${zig_arch}-linux-${ZIG_VERSION}.tar.xz"; \
    curl -fsSL -o "/tmp/${zig_tarball}" \
      "https://ziglang.org/download/${ZIG_VERSION}/${zig_tarball}"; \
    echo "${zig_sha256} */tmp/${zig_tarball}" | sha256sum -c -; \
    tar -xJf "/tmp/${zig_tarball}" -C /usr/local; \
    rm -f "/tmp/${zig_tarball}"; \
    ln -s "/usr/local/zig-${zig_arch}-linux-${ZIG_VERSION}/zig" /usr/local/bin/zig; \
    zig version

# Builds the .deb and .rpm in make package / make dist.
ARG NFPM_TOOL_VERSION=v2.43.3
RUN GOBIN=/usr/local/bin go install github.com/goreleaser/nfpm/v2/cmd/nfpm@${NFPM_TOOL_VERSION} && \
    nfpm --version && go clean -cache -modcache

WORKDIR /src
