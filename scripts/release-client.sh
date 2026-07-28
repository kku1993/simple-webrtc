#!/usr/bin/env bash
# Build the TypeScript client and pack it into an npm tarball.
#
# The tarball is written to dist/ (e.g. dist/simple-webrtc-client-0.1.0.tgz)
# and can be attached to a GitHub release manually, or installed directly with:
#
#   npm install ./dist/simple-webrtc-client-0.1.0.tgz
#
# Usage:
#   scripts/release-client.sh              # build + pack to dist/
#   scripts/release-client.sh --output DIR # place the tarball in DIR
#
# The version is read from the repo-root VERSION file (major.minor.patch) and
# synced into package.json before packing. Override with VERSION=<v>:
#
#   VERSION=0.2.0 scripts/release-client.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLIENT_DIR="${REPO_ROOT}/client"
VERSION_FILE="${REPO_ROOT}/VERSION"
ROOT_PKG="${REPO_ROOT}/package.json"
DIST_DIR="${REPO_ROOT}/dist"

output_dir="${DIST_DIR}"

usage() {
    cat <<'EOF' >&2
Usage: scripts/release-client.sh [--output DIR]

--output DIR
            Directory to place the tarball in. Defaults to <repo>/dist.

The version is read from the repo-root VERSION file. Override with VERSION=<v>:

    VERSION=0.2.0 scripts/release-client.sh
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)    usage ;;
        --output)
            [[ $# -ge 2 ]] || { echo "--output requires an argument" >&2; exit 1; }
            output_dir="$2"
            shift 2
            ;;
        --output=*)
            output_dir="${1#--output=}"
            shift
            ;;
        *)            echo "Unknown argument: $1" >&2; usage ;;
    esac
done

# --- prerequisites -----------------------------------------------------------

for cmd in node npm; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "Missing prerequisite: $cmd" >&2
        exit 1
    fi
done

# --- version -----------------------------------------------------------------

if [[ -n "${VERSION:-}" ]]; then
    version="${VERSION}"
elif [[ -f "${VERSION_FILE}" ]]; then
    version="$(grep -E '^[[:space:]]*[^#[:space:]]' "${VERSION_FILE}" | head -n1 | tr -d '[:space:]')"
    if [[ -z "${version}" ]]; then
        echo "VERSION is empty; refusing to release." >&2
        exit 1
    fi
else
    echo "VERSION not found at ${VERSION_FILE}." >&2
    echo "Set VERSION=<v> in the environment or create the file." >&2
    exit 1
fi

if ! [[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.][0-9A-Za-z.-]+)?$ ]]; then
    echo "Invalid version \"${version}\": expected major.minor.patch (e.g. 0.1.0)." >&2
    exit 1
fi

echo "Releasing @simple-webrtc/client ${version}"
echo "  repo root:  ${REPO_ROOT}"
echo "  client dir: ${CLIENT_DIR}"
echo "  output:     ${output_dir}"
echo

# --- sync version into root package.json -------------------------------------

node -e "
const fs = require('fs');
const pkg = JSON.parse(fs.readFileSync('${ROOT_PKG}', 'utf8'));
if (pkg.version !== '${version}') {
    pkg.version = '${version}';
    fs.writeFileSync('${ROOT_PKG}', JSON.stringify(pkg, null, 2) + '\n');
    console.log('Updated ${ROOT_PKG} version -> ${version}');
} else {
    console.log('${ROOT_PKG} version already ${version}');
}
"
echo

# --- build -------------------------------------------------------------------

echo "Building client..."
cd "${CLIENT_DIR}"
npm install
npm run build

if [[ ! -f "${CLIENT_DIR}/dist/index.js" ]]; then
    echo "Build failed: client/dist/index.js not found." >&2
    exit 1
fi
echo "Build OK: $(ls "${CLIENT_DIR}"/dist/*.js | wc -l) JS files + $(ls "${CLIENT_DIR}"/dist/*.d.ts | wc -l) declaration files"
echo

# --- pack --------------------------------------------------------------------

mkdir -p "${output_dir}"

echo "Packing tarball..."
cd "${REPO_ROOT}"
tarball_name="$(npm pack 2>/dev/null | tail -1)"
if [[ ! -f "${tarball_name}" ]]; then
    echo "npm pack did not produce a tarball." >&2
    exit 1
fi

# Move the tarball to the requested output directory.
if [[ "${output_dir}" != "${REPO_ROOT}" ]]; then
    mv "${tarball_name}" "${output_dir}/"
fi
tarball_path="${output_dir}/${tarball_name}"

echo "Tarball: ${tarball_path} ($(du -h "${tarball_path}" | cut -f1))"
echo
echo "Done. Install locally with:"
echo "  npm install ${tarball_path}"
echo "Or attach ${tarball_path} to a GitHub release for tarball-URL installs."
