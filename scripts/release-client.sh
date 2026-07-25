#!/usr/bin/env bash
# Build the TypeScript client, pack it into an npm tarball, and attach it to
# a GitHub release tagged after server/VERSION (major.minor, e.g. "v0.1").
#
# Consumers then install with:
#
#   npm install https://github.com/<owner>/<repo>/releases/download/v<VERSION>/simple-peer-signal-client-<VERSION>.0.tgz
#
# Usage:
#   scripts/release-client.sh                # build, pack, create release
#   scripts/release-client.sh --update       # upload to an existing release
#   scripts/release-client.sh --dry-run      # build + pack only, no release
#
# Prerequisites:
#   - gh (GitHub CLI) authenticated with repo access
#   - node >= 20, npm
#   - server/VERSION exists (single "major.minor" line)
#
# The npm package version is derived as "<major.minor>.0" (npm requires semver
# major.minor.patch). The git tag is "v<major.minor>" to match server/VERSION.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLIENT_DIR="${REPO_ROOT}/client"
VERSION_FILE="${REPO_ROOT}/server/VERSION"
ROOT_PKG="${REPO_ROOT}/package.json"

dry_run=false
update=false

usage() {
    cat <<'EOF' >&2
Usage: scripts/release-client.sh [--dry-run] [--update]

--dry-run   Build and pack only; do not create or modify a GitHub release.
--update    Upload the tarball to an existing release (instead of creating
            a new one). Use this if the release tag already exists.

The version is read from server/VERSION. Override with VERSION=<v>:

    VERSION=0.2 scripts/release-client.sh
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)    usage ;;
        --dry-run)    dry_run=true;  shift ;;
        --update)     update=true;   shift ;;
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

if ! $dry_run && ! command -v gh >/dev/null 2>&1; then
    echo "Missing prerequisite: gh (GitHub CLI)" >&2
    echo "Install from https://cli.github.com/ and run 'gh auth login'." >&2
    exit 1
fi

# --- version -----------------------------------------------------------------

if [[ -n "${VERSION:-}" ]]; then
    version="${VERSION}"
elif [[ -f "${VERSION_FILE}" ]]; then
    version="$(grep -E '^[[:space:]]*[^#[:space:]]' "${VERSION_FILE}" | head -n1 | tr -d '[:space:]')"
    if [[ -z "${version}" ]]; then
        echo "server/VERSION is empty; refusing to release." >&2
        exit 1
    fi
else
    echo "server/VERSION not found at ${VERSION_FILE}." >&2
    echo "Set VERSION=<v> in the environment or create the file." >&2
    exit 1
fi

if ! [[ "${version}" =~ ^[0-9]+\.[0-9]+$ ]]; then
    echo "Invalid version \"${version}\": expected major.minor (e.g. 0.1)." >&2
    exit 1
fi

npm_version="${version}.0"
tag="v${version}"

echo "Releasing @simple-peer-signal/client ${npm_version} (tag: ${tag})"
echo "  repo root:  ${REPO_ROOT}"
echo "  client dir: ${CLIENT_DIR}"
echo "  dry-run:    ${dry_run}"
echo "  update:     ${update}"
echo

# --- sync version into root package.json -------------------------------------

# Use node to safely update the version field without disturbing formatting.
node -e "
const fs = require('fs');
const pkg = JSON.parse(fs.readFileSync('${ROOT_PKG}', 'utf8'));
if (pkg.version !== '${npm_version}') {
    pkg.version = '${npm_version}';
    fs.writeFileSync('${ROOT_PKG}', JSON.stringify(pkg, null, 2) + '\n');
    console.log('Updated ${ROOT_PKG} version -> ${npm_version}');
} else {
    console.log('${ROOT_PKG} version already ${npm_version}');
}
"

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

echo "Packing tarball..."
cd "${REPO_ROOT}"
tarball="$(npm pack 2>/dev/null | tail -1)"
if [[ ! -f "${tarball}" ]]; then
    echo "npm pack did not produce a tarball." >&2
    exit 1
fi
echo "Tarball: ${tarball} ($(du -h "${tarball}" | cut -f1))"
echo

if $dry_run; then
    echo "Dry run complete. Tarball left at: ${REPO_ROOT}/${tarball}"
    exit 0
fi

# --- release -----------------------------------------------------------------

if $update; then
    echo "Uploading ${tarball} to existing release ${tag}..."
    gh release upload "${tag}" "${tarball}" --clobber
else
    echo "Creating release ${tag} with ${tarball}..."
    gh release create "${tag}" "${tarball}" \
        --title "${tag}" \
        --notes "Client release ${npm_version} (server version ${version})." \
        --generate-notes
fi

rm -f "${tarball}"
echo
echo "Done. Consumers can install with:"
echo
echo "  npm install https://github.com/\$(gh repo view --json nameWithOwner -q .nameWithOwner)/releases/download/${tag}/${tarball}"
