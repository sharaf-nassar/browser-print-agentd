#!/bin/sh
#
# identity.sh — the single identity source for the whole packaging chain.
#
# Every shipped packaging artifact is a `.in` template; build-pkg.sh sources
# this file and renders those templates into its staging directory. Nothing that
# names the product, its bundle id, its binary, or its on-disk directories is
# written by hand anywhere else in this repo — the launchd label, the plist
# filename, the distribution.xml choice/pkg-ref ids, the pre/postinstall LABEL,
# the launcher and uninstaller paths, and the temp-file prefixes all derive from
# the values below. That is the point: a rename is a one-line edit here, not a
# sweep across nine files that can drift apart between releases.
#
# This file is SOURCED, never executed. It must stay POSIX sh with no side
# effects: no command substitution, no output, no `set -e` assumptions. Both
# build-pkg.sh and scripts/check-naming.sh read it, and the latter asserts that
# BINARY_NAME here equals `productName` in identity.go — the Go half of the same
# identity, which cannot be templated because the binary is compiled, not
# rendered.

# The product name. Everything below is derived from it except BUNDLE_PREFIX,
# which is the reverse-DNS namespace the maintainer actually controls
# (sharaf-nassar.github.io), and PRODUCT_TITLE, which is prose.
PRODUCT_NAME="browser-print-agentd"
PRODUCT_TITLE="Browser Print Agent"

# launchd Label and productbuild package identifier — deliberately the same
# string, as they have always been. Free-form reverse-DNS: the hyphen in the
# account name is legal in both, because neither is resolved as a DNS name.
BUNDLE_PREFIX="io.github.sharaf-nassar"
BUNDLE_ID="${BUNDLE_PREFIX}.${PRODUCT_NAME}"

# Installed payload.
BINARY_NAME="${PRODUCT_NAME}"
BINARY_PATH="/usr/local/bin/${BINARY_NAME}"
UNINSTALLER_NAME="${BINARY_NAME}-uninstall"
UNINSTALLER_PATH="/usr/local/bin/${UNINSTALLER_NAME}"
LIBEXEC_DIR="/usr/local/libexec/${PRODUCT_NAME}"
LAUNCHER_PATH="${LIBEXEC_DIR}/launcher"
UPDATER_NAME="updater"
UPDATER_PATH="${LIBEXEC_DIR}/${UPDATER_NAME}"
PLIST_NAME="${BUNDLE_ID}.plist"
AGENT_PLIST_PATH="/Library/LaunchAgents/${PLIST_NAME}"
UPDATER_LABEL="${BUNDLE_ID}.updater"
UPDATER_PLIST_NAME="${UPDATER_LABEL}.plist"
UPDATER_PLIST_PATH="/Library/LaunchDaemons/${UPDATER_PLIST_NAME}"

# Root-owned updater state and logging. These are separate from the agent's
# per-account support and log directories below.
SYSTEM_SUPPORT_DIR="/Library/Application Support/${PRODUCT_NAME}"
UPDATE_STATE_DIR="${SYSTEM_SUPPORT_DIR}/updater"
UPDATE_STATUS_PATH="${SYSTEM_SUPPORT_DIR}/update-status"
SYSTEM_LOG_DIR="/Library/Logs/${PRODUCT_NAME}"

# Per-account directories, relative to a home directory. Names only: the home
# they hang off is resolved at install time against the station account, never
# against root.
SUPPORT_DIR_NAME="${PRODUCT_NAME}"
LOG_DIR_NAME="${PRODUCT_NAME}"
LOG_FILE_NAME="agent.log"

# Environment-variable namespace for the agent and the installer scripts.
ENV_PREFIX="BROWSER_PRINT_AGENTD"
TARGET_USER_ENV="${ENV_PREFIX}_TARGET_USER"
LOG_PATH_ENV="${ENV_PREFIX}_LOG_PATH"

# mktemp/log identity. TEMP_PREFIX is intentionally shorter than PRODUCT_NAME:
# it is the shared prefix for every temp file the product creates, including the
# Go side's spool files.
TEMP_PREFIX="browser-print"

# Public release feed. Derive the GitHub owner from the reverse-DNS namespace
# so the repository identity is not restated in an updater template.
RELEASE_OWNER="${BUNDLE_PREFIX#io.github.}"
RELEASE_REPOSITORY="${RELEASE_OWNER}/${PRODUCT_NAME}"
RELEASE_BASE_URL="https://github.com/${RELEASE_REPOSITORY}/releases"

# Build-time artifact names.
COMPONENT_PKG_NAME="${PRODUCT_NAME}-component.pkg"

# The GUI uninstaller. It ships INSIDE the one installer package, as an app in
# /Applications, because somebody who wants to remove this product looks on
# their own Mac rather than on a downloads page. There is deliberately no second
# package to build, sign, notarize or publish: one product, one installer.
#
# The app owns no removal logic. It runs UNINSTALLER_PATH with one native
# administrator prompt, so the command line and the double-click are the same
# code and cannot drift apart.
UNINSTALL_TITLE="Uninstall ${PRODUCT_TITLE}"
UNINSTALL_APP_NAME="${UNINSTALL_TITLE}.app"
UNINSTALL_APP_PATH="/Applications/${UNINSTALL_APP_NAME}"
UNINSTALL_APP_BUNDLE_ID="${BUNDLE_ID}.uninstall"
# The bundle executable cannot contain spaces as comfortably as the bundle name
# can, and it is never seen by a user.
UNINSTALL_APP_EXEC="${PRODUCT_NAME}-uninstall-app"

# Tag glob that build-pkg.sh derives a default VERSION from. The repo is
# single-product, so releases are plain vX.Y.Z tags.
RELEASE_TAG_GLOB="v*"
RELEASE_TAG_PREFIX="v"
