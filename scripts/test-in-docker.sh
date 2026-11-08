#!/usr/bin/env bash
#
# Run the test suites in a sandboxed Docker container.
#
# Usage: scripts/test-in-docker.sh [--net] [--no-build] [--] [CMD [ARG...]]
#
# Without CMD, runs the whole suite including the integration and lua-engine
# tests. The container sees the source tree read-only, runs as an unprivileged
# user without capabilities and, by default, without network. The only
# writable places are /tmp (a tmpfs holding TMPDIR) and the Go build cache
# volume; HOME is read-only. A write anywhere else fails with EROFS or EACCES,
# and anything a command leaves behind in /tmp or /dev/shm fails the run, so
# stray writes surface as failures instead of vanishing with the container.
#
# Options:
#   --net       Give the container network access (also: NET=1). Needed by
#               the network-gated tests, which reach luarocks.org.
#   --no-build  Use the existing image instead of (re)building it.
#
# Environment:
#   GO_VERSION, DEBIAN_SUITE, TARANTOOL_VERSION
#               Override the toolchain versions pinned in test/docker/Dockerfile.
#   IMAGE       Image tag to build and run (default: go-luarocks-sandbox).
#   TMPFS_SIZE  Size of the /tmp tmpfs (default: 4g).
#
# Examples:
#   scripts/test-in-docker.sh
#   scripts/test-in-docker.sh --net
#   scripts/test-in-docker.sh go test -count=1 -v ./build/...

set -euo pipefail

# Turns tests' missing-prerequisite skips into failures: the image provides
# every prerequisite, so a skip there means a broken environment.
STRICT_ENV=GO_LUAROCKS_INTEGRATION_STRICT

# Tell tests they run inside the sandbox; GO_LUAROCKS_OFFLINE is set
# additionally when the container has no network.
SANDBOX_ENV=GO_LUAROCKS_SANDBOX
OFFLINE_ENV=GO_LUAROCKS_OFFLINE

# The module's packages: PKGS of .github/workflows/ci.yml.
PKGS=(./build/... ./client/... ./deps/... ./fetch/... ./internal/... ./manif/...
	./remote/... ./rockspec/... ./tree/... .)
TAGS=integration,integration_luaengine

IMAGE=${IMAGE:-go-luarocks-sandbox}
TMPFS_SIZE=${TMPFS_SIZE:-4g}
GOCACHE_VOLUME=go-luarocks-sandbox-gocache
GOMODCACHE_VOLUME=go-luarocks-sandbox-gomodcache
NET=${NET:-0}
BUILD=1

usage() {
	sed -n '3,/^$/s/^# \{0,1\}//p' "$0"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--net) NET=1; shift ;;
	--no-build) BUILD=0; shift ;;
	-h | --help) usage; exit 0 ;;
	--) shift; break ;;
	-*) echo "test-in-docker: unknown option $1" >&2; usage >&2; exit 2 ;;
	*) break ;;
	esac
done

# The client package alone runs its parity and interop suites for about seven
# minutes, close to go test's 10m default, and longer on a loaded machine.
if [ "$#" -eq 0 ]; then
	set -- go test -count=1 -timeout 45m -tags "$TAGS" "${PKGS[@]}"
fi

repo=$(cd "$(dirname "$0")/.." && pwd -P)

if [ "$BUILD" = 1 ]; then
	build_args=()
	for arg in GO_VERSION DEBIAN_SUITE TARANTOOL_VERSION; do
		if [ -n "${!arg:-}" ]; then
			build_args+=(--build-arg "$arg=${!arg}")
		fi
	done
	docker build "${build_args[@]}" -t "$IMAGE" "$repo/test/docker" >&2
fi

sandbox=(
	--rm --init
	--read-only
	--tmpfs "/tmp:rw,exec,nosuid,nodev,size=$TMPFS_SIZE"
	--volume "$repo:/src:ro"
	--workdir /src
	--volume "$GOCACHE_VOLUME:/cache/go-build"
	--cap-drop ALL
	--security-opt no-new-privileges
)

# Fill the module cache volume. Only `go mod download` ever gets network for
# this, and only when the offline check finds a module missing.
if ! docker run "${sandbox[@]}" --network none \
	--volume "$GOMODCACHE_VOLUME:/cache/gomod" \
	--env GOPROXY=off "$IMAGE" go mod download >/dev/null 2>&1; then
	echo "test-in-docker: downloading modules into volume $GOMODCACHE_VOLUME" >&2
	docker run "${sandbox[@]}" \
		--volume "$GOMODCACHE_VOLUME:/cache/gomod" \
		"$IMAGE" go mod download
fi

run=(
	"${sandbox[@]}"
	--volume "$GOMODCACHE_VOLUME:/cache/gomod:ro"
	--env "$SANDBOX_ENV=1"
	--env "$STRICT_ENV=1"
)
if [ "$NET" = 1 ]; then
	echo "test-in-docker: running with network access" >&2
else
	run+=(--network none --env "$OFFLINE_ENV=1" --env GOPROXY=off)
fi

exec docker run "${run[@]}" "$IMAGE" "$@"
