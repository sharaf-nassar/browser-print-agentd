#!/usr/bin/env bash
#
# build-pkg.sh — build the station installer for the macOS print agent.
#
# Produces, in order:
#
#   1. a darwin/<arch> binary, cross-compiled (no Xcode, no Mac needed);
#   2. a staged payload tree plus the pre/postinstall scripts, RENDERED from the
#      packaging/*.in templates against packaging/identity.sh;
#   3. a component package via `pkgbuild`;
#   4. a distribution package via `productbuild`;
#   5. optionally a signed distribution package via `productsign`.
#
# Stages 1 and 2 run anywhere Go runs, which is what --stage-only exposes: the
# whole layout — install paths, file modes, script set — is verifiable on Linux
# CI with no Apple hardware. Stages 3 onward need macOS, because pkgbuild,
# productbuild and productsign have no portable equivalent.
#
# Identity is templated, not typed twice. packaging/identity.sh is the single
# source for the product name, the bundle id (which is both the launchd Label
# and the pkg identifier), the installed paths, and the per-account directory
# names. Every shipped artifact — the LaunchAgent plist, distribution.xml, the
# pre/postinstall scripts, the launcher, the uninstaller — is a `.in` template
# rendered here with sed. Nothing in the staging tree is copied verbatim, and a
# template that still carries an unrendered __PLACEHOLDER__ fails the build
# rather than shipping. The generated plist is named ${BUNDLE_ID}.plist.
#
# Signing is opt-in through the identity flags/environment rather than assumed,
# so a local build works unsigned while the release workflow passes the
# Developer ID identities it imported from GitHub secrets. An unsigned package
# is named accordingly and cannot be mistaken for a shippable artifact: only a
# productsign'd, notarized, stapled .pkg installs Gatekeeper-clean on a station.
#
# Usage:
#   ./build-pkg.sh [options]
#
#   --version <v>              Package version (default: derived from the newest
#                              vX.Y.Z tag, else 0.0.0-dev).
#   --arch <arm64|amd64>       Go architecture to build (default: arm64).
#   --output <dir>             Where the .pkg lands (default: <packaging>/dist).
#   --app-identity <id>        "Developer ID Application" identity; codesigns
#                              the binary with the hardened runtime.
#   --installer-identity <id>  "Developer ID Installer" identity; productsigns
#                              the distribution package.
#   --stage-only               Build and stage, then stop before pkgbuild.
#   -h, --help                 This message.
#
# Environment equivalents: VERSION, ARCH, OUTPUT_DIR, APP_SIGNING_IDENTITY,
# INSTALLER_SIGNING_IDENTITY, STAGE_ONLY, GO_LDFLAGS.

set -euo pipefail

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "$PACKAGING_DIR/.." && pwd)"

# The single identity source. Everything named below comes from here; this
# script defines no product string of its own.
# shellcheck source=identity.sh
. "$PACKAGING_DIR/identity.sh"

PKG_IDENTIFIER="$BUNDLE_ID"

VERSION="${VERSION:-}"
ARCH="${ARCH:-arm64}"
OUTPUT_DIR="${OUTPUT_DIR:-$PACKAGING_DIR/dist}"
APP_SIGNING_IDENTITY="${APP_SIGNING_IDENTITY:-}"
INSTALLER_SIGNING_IDENTITY="${INSTALLER_SIGNING_IDENTITY:-}"
STAGE_ONLY="${STAGE_ONLY:-0}"
# Empty by default: the release workflow injects `-X` version stamping here once
# the binary carries a version symbol, and nothing else needs link flags.
GO_LDFLAGS="${GO_LDFLAGS:-}"

log() { printf '[build-pkg] %s\n' "$*"; }
fail() {
	printf '[build-pkg] ERROR: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<-EOF
		Usage: ./build-pkg.sh [options]

		  --version <v>              Package version (default: newest vX.Y.Z tag,
		                             else 0.0.0-dev).
		  --arch <arm64|amd64>       Go architecture to build (default: arm64).
		  --output <dir>             Where the .pkg lands (default: ./dist).
		  --app-identity <id>        "Developer ID Application" identity; codesigns
		                             the binary with the hardened runtime.
		  --installer-identity <id>  "Developer ID Installer" identity; productsigns
		                             the distribution package.
		  --stage-only               Build and stage, then stop before pkgbuild.
		                             The only mode that runs off macOS.
		  -h, --help                 This message.

		Environment equivalents: VERSION, ARCH, OUTPUT_DIR, APP_SIGNING_IDENTITY,
		INSTALLER_SIGNING_IDENTITY, STAGE_ONLY, GO_LDFLAGS.
	EOF
	exit "${1:-0}"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version)
		[ "$#" -ge 2 ] || fail "--version needs a value"
		VERSION="$2"
		shift 2
		;;
	--arch)
		[ "$#" -ge 2 ] || fail "--arch needs a value"
		ARCH="$2"
		shift 2
		;;
	--output)
		[ "$#" -ge 2 ] || fail "--output needs a value"
		OUTPUT_DIR="$2"
		shift 2
		;;
	--app-identity)
		[ "$#" -ge 2 ] || fail "--app-identity needs a value"
		APP_SIGNING_IDENTITY="$2"
		shift 2
		;;
	--installer-identity)
		[ "$#" -ge 2 ] || fail "--installer-identity needs a value"
		INSTALLER_SIGNING_IDENTITY="$2"
		shift 2
		;;
	--stage-only)
		STAGE_ONLY=1
		shift
		;;
	-h | --help)
		usage 0
		;;
	*)
		printf 'unknown argument: %s\n' "$1" >&2
		usage 2
		;;
	esac
done

case "$ARCH" in
arm64) HOST_ARCHITECTURES="arm64" ;;
amd64) HOST_ARCHITECTURES="x86_64" ;;
*) fail "unsupported --arch '$ARCH' (expected arm64 or amd64)" ;;
esac

if [ -z "$VERSION" ]; then
	# Releases are cut from vX.Y.Z tags; a working tree with no such tag builds a
	# clearly-marked dev package instead of guessing a version.
	tag="$(git -C "$AGENT_DIR" describe --tags --abbrev=0 --match "$RELEASE_TAG_GLOB" 2>/dev/null || true)"
	if [ -n "$tag" ]; then
		VERSION="${tag#"$RELEASE_TAG_PREFIX"}"
	else
		VERSION="0.0.0-dev"
	fi
fi

command -v go >/dev/null 2>&1 || fail "go is not on PATH"

if [ "$STAGE_ONLY" != "1" ] && [ "$(uname -s)" != "Darwin" ]; then
	fail "pkgbuild/productbuild/productsign only exist on macOS. Re-run with --stage-only to verify the payload layout here, and build the .pkg on a macOS runner."
fi

STAGE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/$TEMP_PREFIX-pkg.XXXXXX")"
trap 'rm -rf "$STAGE_DIR"' EXIT

PAYLOAD_DIR="$STAGE_DIR/payload"
SCRIPTS_DIR="$STAGE_DIR/scripts"
COMPONENTS_DIR="$STAGE_DIR/components"

log "version $VERSION, arch $ARCH ($HOST_ARCHITECTURES), staging in $STAGE_DIR"
log "identity: $PRODUCT_NAME / $BUNDLE_ID"

# ---------------------------------------------------------------------------
# 1. Build the binary
# ---------------------------------------------------------------------------

mkdir -p "$PAYLOAD_DIR/usr/local/bin" \
	"$PAYLOAD_DIR$LIBEXEC_DIR" \
	"$PAYLOAD_DIR/Library/LaunchAgents" \
	"$SCRIPTS_DIR" "$COMPONENTS_DIR" "$OUTPUT_DIR"

log "building $BINARY_NAME for darwin/$ARCH"
# An array rather than an unquoted expansion, so a multi-word GO_LDFLAGS reaches
# `go build` as ONE argument. The `${x[@]+...}` guard is for bash 3.2, which is
# what /bin/bash still is on macOS and where an empty array trips `set -u`.
ldflags_args=()
if [ -n "$GO_LDFLAGS" ]; then
	ldflags_args=(-ldflags "$GO_LDFLAGS")
fi
(
	cd "$AGENT_DIR"
	# CGO off keeps the Mach-O self-contained (one file to sign, no dylib
	# forest); -trimpath keeps build paths out of the shipped binary;
	# -buildvcs=false so VCS stamping can never fail a packaging build.
	GOOS=darwin GOARCH="$ARCH" CGO_ENABLED=0 \
		go build -trimpath -buildvcs=false \
		${ldflags_args[@]+"${ldflags_args[@]}"} \
		-o "$PAYLOAD_DIR$BINARY_PATH" ./...
)

# ---------------------------------------------------------------------------
# 2. Render the templates into the staging tree
# ---------------------------------------------------------------------------

# Every shipped artifact goes through here. `|` is the sed delimiter because
# half the values are absolute paths; the substitution list is the complete
# public surface of identity.sh plus the one build-derived value.
render_template() {
	local src="$1" dest="$2" mode="$3" residue
	[ -f "$src" ] || fail "missing template: $src"
	mkdir -p "$(dirname "$dest")"
	sed \
		-e "s|__PRODUCT_NAME__|$PRODUCT_NAME|g" \
		-e "s|__PRODUCT_TITLE__|$PRODUCT_TITLE|g" \
		-e "s|__BUNDLE_ID__|$BUNDLE_ID|g" \
		-e "s|__BINARY_NAME__|$BINARY_NAME|g" \
		-e "s|__BINARY_PATH__|$BINARY_PATH|g" \
		-e "s|__UNINSTALLER_NAME__|$UNINSTALLER_NAME|g" \
		-e "s|__UNINSTALLER_PATH__|$UNINSTALLER_PATH|g" \
		-e "s|__LIBEXEC_DIR__|$LIBEXEC_DIR|g" \
		-e "s|__LAUNCHER_PATH__|$LAUNCHER_PATH|g" \
		-e "s|__PLIST_NAME__|$PLIST_NAME|g" \
		-e "s|__AGENT_PLIST_PATH__|$AGENT_PLIST_PATH|g" \
		-e "s|__SUPPORT_DIR_NAME__|$SUPPORT_DIR_NAME|g" \
		-e "s|__LOG_DIR_NAME__|$LOG_DIR_NAME|g" \
		-e "s|__ENV_PREFIX__|$ENV_PREFIX|g" \
		-e "s|__TARGET_USER_ENV__|$TARGET_USER_ENV|g" \
		-e "s|__TEMP_PREFIX__|$TEMP_PREFIX|g" \
		-e "s|__COMPONENT_PKG_NAME__|$COMPONENT_PKG_NAME|g" \
		-e "s|__HOST_ARCHITECTURES__|$HOST_ARCHITECTURES|g" \
		"$src" >"$dest"
	chmod "$mode" "$dest"
	# A placeholder that survives rendering is a template referring to an
	# identity field that does not exist. Shipping it would put a literal
	# __NAME__ into a launchd plist or an installer script, so it fails here.
	residue="$(grep -n '__[A-Z][A-Z0-9_]*__' "$dest" || true)"
	if [ -n "$residue" ]; then
		printf '%s\n' "$residue" >&2
		fail "$dest still contains unrendered placeholders (see above)"
	fi
}

render_template "$PACKAGING_DIR/launcher.sh.in" \
	"$PAYLOAD_DIR$LAUNCHER_PATH" 755
render_template "$PACKAGING_DIR/uninstall.sh.in" \
	"$PAYLOAD_DIR$UNINSTALLER_PATH" 755
render_template "$PACKAGING_DIR/launchagent.plist.in" \
	"$PAYLOAD_DIR$AGENT_PLIST_PATH" 644
render_template "$PACKAGING_DIR/scripts/preinstall.in" "$SCRIPTS_DIR/preinstall" 755
render_template "$PACKAGING_DIR/scripts/postinstall.in" "$SCRIPTS_DIR/postinstall" 755
render_template "$PACKAGING_DIR/distribution.xml.in" "$STAGE_DIR/distribution.xml" 644
chmod 755 "$PAYLOAD_DIR$BINARY_PATH"

# Belt and braces over the whole staged tree, not just the files rendered above:
# a template that is added later and copied by hand would otherwise slip through.
unrendered="$(grep -rIln '__[A-Z][A-Z0-9_]*__' "$PAYLOAD_DIR" "$SCRIPTS_DIR" "$STAGE_DIR/distribution.xml" 2>/dev/null || true)"
if [ -n "$unrendered" ]; then
	printf '%s\n' "$unrendered" >&2
	fail "the staged tree contains unrendered placeholders in the files listed above"
fi

log "staged payload:"
(cd "$PAYLOAD_DIR" && find . -type f -exec ls -l {} \; | sed 's/^/    /')
log "staged scripts:"
(cd "$SCRIPTS_DIR" && find . -type f -exec ls -l {} \; | sed 's/^/    /')

if [ "$STAGE_ONLY" = "1" ]; then
	# Copy the staged tree out before the EXIT trap deletes it, so a CI job can
	# assert against the real layout rather than this script's own logging.
	staged_copy="$OUTPUT_DIR/stage-$VERSION-$ARCH"
	rm -rf "$staged_copy"
	mkdir -p "$staged_copy"
	cp -R "$PAYLOAD_DIR" "$SCRIPTS_DIR" "$STAGE_DIR/distribution.xml" "$staged_copy/"
	log "stage-only run complete: $staged_copy"
	exit 0
fi

# ---------------------------------------------------------------------------
# 3. Sign the binary (optional, macOS only)
# ---------------------------------------------------------------------------

plutil -lint "$PAYLOAD_DIR$AGENT_PLIST_PATH" >/dev/null ||
	fail "the LaunchAgent plist failed plutil -lint"

if [ -n "$APP_SIGNING_IDENTITY" ]; then
	log "codesigning the binary with the hardened runtime"
	# Hardened runtime plus a secure timestamp are both notarization
	# preconditions; without either, notarytool rejects the package payload.
	codesign --force --timestamp --options runtime \
		--sign "$APP_SIGNING_IDENTITY" "$PAYLOAD_DIR$BINARY_PATH"
	codesign --verify --strict --verbose=2 "$PAYLOAD_DIR$BINARY_PATH"
else
	log "no --app-identity given: the binary is UNSIGNED and will not pass Gatekeeper"
fi

# ---------------------------------------------------------------------------
# 4. Component + distribution package
# ---------------------------------------------------------------------------

log "building the component package"
# --ownership recommended: the payload installs root-owned regardless of who
# built it, which launchd requires of anything in /Library/LaunchAgents.
pkgbuild \
	--root "$PAYLOAD_DIR" \
	--scripts "$SCRIPTS_DIR" \
	--identifier "$PKG_IDENTIFIER" \
	--version "$VERSION" \
	--install-location / \
	--ownership recommended \
	"$COMPONENTS_DIR/$COMPONENT_PKG_NAME"

unsigned_pkg="$STAGE_DIR/$BINARY_NAME-$VERSION-unsigned.pkg"
log "building the distribution package"
productbuild \
	--distribution "$STAGE_DIR/distribution.xml" \
	--package-path "$COMPONENTS_DIR" \
	"$unsigned_pkg"

# ---------------------------------------------------------------------------
# 5. Sign the package (optional)
# ---------------------------------------------------------------------------

if [ -n "$INSTALLER_SIGNING_IDENTITY" ]; then
	final_pkg="$OUTPUT_DIR/$BINARY_NAME-$VERSION.pkg"
	log "productsigning the distribution package"
	productsign --sign "$INSTALLER_SIGNING_IDENTITY" "$unsigned_pkg" "$final_pkg"
	pkgutil --check-signature "$final_pkg"
else
	final_pkg="$OUTPUT_DIR/$BINARY_NAME-$VERSION-unsigned.pkg"
	cp "$unsigned_pkg" "$final_pkg"
	log "no --installer-identity given: the package is UNSIGNED. It cannot be notarized and will not install on a station."
fi

log "built $final_pkg"
