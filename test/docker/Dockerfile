# Sandbox image for the black-box test suites (integration, backend parity).
#
# The image only carries the toolchain; the source tree is bind-mounted
# read-only at run time by scripts/test-in-docker.sh, which also applies the
# sandbox (read-only root, no network, no capabilities). Build it with this
# directory as the context, e.g.:
#
#   docker build -t go-luarocks-sandbox test/docker
#
# Versions are pinned by ARGs so a rebuild yields the same Go and Tarantool;
# override them with --build-arg (GO_VERSION=1.26.8 tests against the minimum
# Go of go.mod).

# Current stable Go. go.mod requires 1.26 or later; CI tests the previous and
# the current minor release.
ARG GO_VERSION=1.27.3
# Debian release the golang image is built on; cmake, gcc, make and git come
# from its archive, so the suite pins their major versions. Not trixie: its apt
# (sqv) rejects the Tarantool repository key, whose binding signatures use
# SHA-1.
ARG DEBIAN_SUITE=bookworm

FROM golang:${GO_VERSION}-${DEBIAN_SUITE}

# Tarantool release; tarantool-dev puts lua.h at /usr/include/tarantool, which
# is <prefix>/include/tarantool for prefix = dirname(dirname(which tarantool)),
# the layout the lua-engine tests expect. The "static" linux-deb repository
# carries distro-independent builds.
ARG TARANTOOL_VERSION=3.9.0
ARG TARANTOOL_SERIES=series-3
ARG TARANTOOL_REPO=https://download.tarantool.org/tarantool/release
# UID of the unprivileged test user. Its value is irrelevant to the sandbox
# (the source is mounted read-only), so a fixed one keeps the image portable.
ARG TEST_UID=10001

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

RUN install -d -m 0755 /etc/apt/keyrings \
    && curl -fsSL "${TARANTOOL_REPO}/${TARANTOOL_SERIES}/gpgkey" \
        -o /etc/apt/keyrings/tarantool.asc \
    && echo "deb [signed-by=/etc/apt/keyrings/tarantool.asc] ${TARANTOOL_REPO}/${TARANTOOL_SERIES}/linux-deb/ static main" \
        > /etc/apt/sources.list.d/tarantool.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        cmake \
        gcc \
        git \
        libc6-dev \
        make \
        tarantool="${TARANTOOL_VERSION}-1" \
        tarantool-dev="${TARANTOOL_VERSION}-1" \
        unzip \
    && rm -rf /var/lib/apt/lists/* \
    && test -f "$(dirname "$(dirname "$(command -v tarantool)")")/include/tarantool/lua.h"

# The test user owns nothing but the cache directories, which become named
# volumes; a fresh volume inherits this ownership. Its HOME is owned by root
# and the root filesystem is mounted read-only, so any write there fails.
RUN useradd --uid "${TEST_UID}" --user-group --no-create-home \
        --home-dir /home/tester --shell /bin/bash tester \
    && install -d -m 0755 -o root -g root /home/tester \
    && install -d -m 0755 -o tester -g tester /cache /cache/go-build /cache/gomod

# Kept executable in git; no --chmod, which the legacy builder lacks.
COPY sandbox-run /usr/local/bin/sandbox-run

ENV HOME=/home/tester \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/gomod \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    TMPDIR=/tmp/gotest

USER tester
WORKDIR /src
ENTRYPOINT ["/usr/local/bin/sandbox-run"]
