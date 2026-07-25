#!/usr/bin/env bash
# Build a statically linked, CGO_ENABLED=0 Go binary for simple-peer-signal-server.
#
# Usage:
#   scripts/build.sh              # build for the current host architecture
#   scripts/build.sh x86_64       # cross-compile for linux/amd64
#   scripts/build.sh arm64        # cross-compile for linux/arm64
#   scripts/build.sh --output DIR # place the binary in DIR (default: scripts/../dist)
#
# The version is read from server/VERSION (a single "major.minor" line) and
# stamped into the binary via -ldflags "-X ...version.Version=<v>". The binary
# is named `simple-peer-signal-server-<version>-<arch>` (e.g. simple-peer-signal-server-0.1.0-x86_64).
# The resulting binary is fully static (no libc dependency) because CGO is disabled.

set -euo pipefail

# Resolve the repository root from the script location so it can be invoked
# from any working directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SERVER_DIR="${REPO_ROOT}/server"
DIST_DIR="${REPO_ROOT}/dist"

arch=""
output_dir="${DIST_DIR}"

usage() {
    cat <<'EOF' >&2
Usage: scripts/build.sh [arch] [--output DIR]

arch        Target architecture: x86_64 (linux/amd64) or arm64 (linux/arm64).
            Defaults to the host architecture.

--output DIR
            Directory to place the binary in. Defaults to <repo>/dist.

The version is read from server/VERSION. Override with VERSION=<v>:

    VERSION=0.2 scripts/build.sh arm64

Examples:
    scripts/build.sh
    scripts/build.sh x86_64
    scripts/build.sh arm64 --output /tmp/out
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)
            usage
            ;;
        --output)
            [[ $# -ge 2 ]] || { echo "--output requires an argument" >&2; exit 1; }
            output_dir="$2"
            shift 2
            ;;
        --output=*)
            output_dir="${1#--output=}"
            shift
            ;;
        x86_64|amd64)
            arch="x86_64"
            shift
            ;;
        arm64|aarch64)
            arch="arm64"
            shift
            ;;
        *)
            echo "Unknown argument: $1" >&2
            usage
            ;;
    esac
done

# Derive the target architecture from the host if not specified.
if [[ -z "${arch}" ]]; then
    case "$(uname -m)" in
        x86_64|amd64)   arch="x86_64" ;;
        arm64|aarch64)  arch="arm64" ;;
        *)
            echo "Unsupported host architecture: $(uname -m)" >&2
            echo "Pass 'x86_64' or 'arm64' explicitly." >&2
            exit 1
            ;;
    esac
fi

# Map the friendly arch name to a Go GOARCH.
case "${arch}" in
    x86_64) goarch="amd64" ;;
    arm64)  goarch="arm64" ;;
    *) echo "Internal error: unhandled arch ${arch}" >&2; exit 1 ;;
esac

if [[ ! -d "${SERVER_DIR}" ]]; then
    echo "Server source directory not found: ${SERVER_DIR}" >&2
    exit 1
fi

# Read the version from server/VERSION unless overridden via the VERSION env
# var. The file is expected to contain a single line of the form "major.minor".
version_file="${SERVER_DIR}/VERSION"
if [[ -n "${VERSION:-}" ]]; then
    version="${VERSION}"
elif [[ -f "${version_file}" ]]; then
    # Strip whitespace/comments; take the first non-empty, non-# line.
    version="$(grep -E '^[[:space:]]*[^#[:space:]]' "${version_file}" | head -n1 | tr -d '[:space:]')"
    if [[ -z "${version}" ]]; then
        echo "server/VERSION is empty; refusing to build." >&2
        exit 1
    fi
else
    echo "server/VERSION not found at ${version_file}." >&2
    echo "Set VERSION=<v> in the environment or create the file." >&2
    exit 1
fi

# Validate the version looks like major.minor (allowing optional patch/pre).
if ! [[ "${version}" =~ ^[0-9]+\.[0-9]+([.][0-9A-Za-z.-]+)?$ ]]; then
    echo "Invalid version \"${version}\": expected major.minor (e.g. 0.1)." >&2
    exit 1
fi

mkdir -p "${output_dir}"

binary_name="simple-peer-signal-server-${version}-${arch}"
output_path="${output_dir}/${binary_name}"

echo "Building ${binary_name} (GOOS=linux GOARCH=${goarch}, CGO_ENABLED=0, static)..."
echo "  module:  ${SERVER_DIR}"
echo "  version: ${version}"
echo "  output:  ${output_path}"

# Disable cgo for a fully static binary. Trim the file path information for
# reproducible builds and strip the symbol table / DWARF table to shrink the
# resulting artifact. The version is stamped into the version package via
# -X so `--version` and the startup log line report it.
version_pkg="github.com/kku1993/simple-peer-signal-server/internal/version"

export CGO_ENABLED=0
export GOOS=linux
export GOARCH="${goarch}"
export GOFLAGS="${GOFLAGS:-} -trimpath"
export CGO_LDFLAGS=""

cd "${SERVER_DIR}"
go build \
    -trimpath \
    -ldflags "-s -w -extldflags '-static' -X ${version_pkg}.Version=${version}" \
    -o "${output_path}" \
    ./cmd/server

# Verify the binary is statically linked when `file` is available.
if command -v file >/dev/null 2>&1; then
    file_info="$(file "${output_path}")"
    echo "  file:   ${file_info}"
    if [[ "${file_info}" == *"statically linked"* || "${file_info}" == *"shared object"* && "${file_info}" != *"dynamically"* ]]; then
        :
    fi
    if [[ "${file_info}" == *"dynamically linked"* ]]; then
        echo "WARNING: binary appears to be dynamically linked." >&2
        exit 1
    fi
fi

echo "Done: ${output_path}"
