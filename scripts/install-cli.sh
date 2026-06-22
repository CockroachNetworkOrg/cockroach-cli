#!/bin/sh
# =============================================================================
# install-cli.sh — install the cockroach-cli binary from a GitHub Release.
# Detects OS/arch, downloads the matching archive + checksums.txt, verifies the
# sha256, extracts, and installs cockroach-cli onto your PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cockroachnetworkorg/cockroach-cli/main/scripts/install-cli.sh | sh
#
# Pin a version (recommended for prod / CI):
#   curl -fsSL .../install-cli.sh | VERSION=v1.2.3 sh
#
# Install somewhere other than /usr/local/bin:
#   curl -fsSL .../install-cli.sh | PREFIX="$HOME/.local" sh   # -> $HOME/.local/bin
#
# Env knobs:
#   VERSION   release tag to install (default: latest)
#   PREFIX    install root; binary lands in $PREFIX/bin (default: /usr/local)
#   REPO      owner/name to pull releases from (default: cockroachnetworkorg/cockroach-cli)
#
# POSIX sh, no bashisms. Mirrors the shape of rustup / get-helm-3: fail loudly,
# verify checksums, leave nothing behind. Idempotent — re-running overwrites the
# existing binary in place.
# =============================================================================
set -eu

REPO="${REPO:-cockroachnetworkorg/cockroach-cli}"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
BIN_NAME="cockroach-cli"

# --- helpers ----------------------------------------------------------------
info() { printf '%s\n' "==> $*"; }
warn() { printf '%s\n' "warning: $*" >&2; }
err() {
	printf '%s\n' "error: $*" >&2
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

# A downloader: prefer curl, fall back to wget. Writes URL ($1) -> file ($2).
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		err "need curl or wget to download files"
	fi
}

# Fetch a URL to stdout (used for the GitHub API + redirect resolution).
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	else
		wget -qO- "$1"
	fi
}

need tar
need uname

# --- detect OS --------------------------------------------------------------
os="$(uname -s)"
case "$os" in
Linux) OS="linux" ;;
Darwin) OS="darwin" ;;
MINGW* | MSYS* | CYGWIN* | Windows_NT) OS="windows" ;;
*) err "unsupported OS: $os (linux, darwin, windows only)" ;;
esac

# --- detect arch (normalise to GoReleaser's amd64/arm64) --------------------
arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
*) err "unsupported architecture: $arch (amd64, arm64 only)" ;;
esac

# Archive extension matches the GoReleaser config: zip on Windows, else tar.gz.
if [ "$OS" = "windows" ]; then EXT="zip"; else EXT="tar.gz"; fi

# --- resolve the release tag ------------------------------------------------
if [ -n "${VERSION:-}" ]; then
	TAG="$VERSION"
	info "Installing pinned version: $TAG"
else
	# Resolve the latest tag WITHOUT the API (avoids the unauthenticated rate
	# limit): the /releases/latest page 302-redirects to /releases/tag/<tag>.
	info "Resolving latest release of $REPO ..."
	if command -v curl >/dev/null 2>&1; then
		loc="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
			"https://github.com/$REPO/releases/latest")"
	else
		# wget prints the final URL on stderr while spidering.
		loc="$(wget -q --spider -S "https://github.com/$REPO/releases/latest" 2>&1 |
			awk '/^  Location: /{u=$2} END{print u}')"
		[ -n "$loc" ] || loc="https://github.com/$REPO/releases/latest"
	fi
	TAG="${loc##*/tag/}"
	case "$TAG" in
	v*) : ;; # looks like a tag, good
	*) err "could not resolve latest release tag (got '$TAG'); pin one with VERSION=vX.Y.Z" ;;
	esac
	info "Latest release: $TAG"
fi

# Version string inside the archive name has no leading 'v' (GoReleaser .Version).
VER="${TAG#v}"
ARCHIVE="${BIN_NAME}_${VER}_${OS}_${ARCH}.${EXT}"
BASE_URL="https://github.com/$REPO/releases/download/$TAG"

# --- work in a self-cleaning temp dir ---------------------------------------
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t cockroach-cli)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

info "Downloading $ARCHIVE ..."
download "$BASE_URL/$ARCHIVE" "$TMP/$ARCHIVE" ||
	err "download failed: $BASE_URL/$ARCHIVE (no build for $OS/$ARCH at $TAG?)"

info "Downloading checksums.txt ..."
download "$BASE_URL/checksums.txt" "$TMP/checksums.txt" ||
	err "could not download checksums.txt from $BASE_URL"

# --- verify sha256 (fail LOUDLY on mismatch) --------------------------------
info "Verifying sha256 ..."
expected="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
[ -n "$expected" ] || err "no checksum entry for $ARCHIVE in checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$TMP/$ARCHIVE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{print $1}')"
else
	err "need sha256sum or shasum to verify the download"
fi

if [ "$expected" != "$actual" ]; then
	err "CHECKSUM MISMATCH for $ARCHIVE
  expected: $expected
  actual:   $actual
Refusing to install. The download may be corrupt or tampered with."
fi
info "Checksum OK."

# --- extract ----------------------------------------------------------------
info "Extracting ..."
if [ "$EXT" = "zip" ]; then
	need unzip
	unzip -oq "$TMP/$ARCHIVE" -d "$TMP/extract"
else
	mkdir -p "$TMP/extract"
	tar -xzf "$TMP/$ARCHIVE" -C "$TMP/extract"
fi

# The binary may be at the archive root or nested; find it.
SRC="$(find "$TMP/extract" -type f -name "$BIN_NAME" -o -type f -name "$BIN_NAME.exe" 2>/dev/null | head -n1)"
[ -n "$SRC" ] || err "could not find $BIN_NAME inside the archive"
chmod +x "$SRC"

# --- install ----------------------------------------------------------------
# sudo only if we can't write the target dir ourselves (rustup-style).
SUDO=""
if [ ! -d "$BIN_DIR" ]; then
	mkdir -p "$BIN_DIR" 2>/dev/null || SUDO="sudo"
fi
if [ -n "$SUDO" ] || { [ -d "$BIN_DIR" ] && [ ! -w "$BIN_DIR" ]; }; then
	SUDO="sudo"
	command -v sudo >/dev/null 2>&1 || err "$BIN_DIR is not writable and sudo is unavailable; re-run with PREFIX=\$HOME/.local"
fi

DEST="$BIN_DIR/$(basename "$SRC")"
info "Installing to $DEST ..."
${SUDO:+$SUDO} mkdir -p "$BIN_DIR"
${SUDO:+$SUDO} install -m 0755 "$SRC" "$DEST" 2>/dev/null ||
	{ ${SUDO:+$SUDO} cp "$SRC" "$DEST" && ${SUDO:+$SUDO} chmod 0755 "$DEST"; }

# --- confirm ----------------------------------------------------------------
info "Installed cockroach-cli $TAG"
if command -v "$BIN_NAME" >/dev/null 2>&1; then
	"$BIN_NAME" version || true
else
	warn "$BIN_DIR is not on your PATH. Add it, e.g.:"
	printf '  export PATH="%s:$PATH"\n' "$BIN_DIR"
	"$DEST" version || true
fi
