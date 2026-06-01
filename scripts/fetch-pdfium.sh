#!/usr/bin/env bash
# Fetches the prebuilt PDFium shared library for this host from
# bblanchon/pdfium-binaries into third_party/pdfium/, so attachment
# rasterization works out of the box (the app loads it at runtime — see
# src/tools/pdfrender.rs). Idempotent: skips the download if the lib is present.
#
# Supply-chain hardening: the release is PINNED to a specific tag (not
# `latest`) and every archive is verified against a checked-in SHA-256 before
# extraction, so a tampered or swapped binary fails the build instead of being
# silently loaded into the process.
set -euo pipefail

# Pinned bblanchon/pdfium-binaries release. Bump deliberately, then refresh the
# checksums below (download each asset from the new tag and `shasum -a 256`).
PDFIUM_VERSION="chromium/7869"

DIR="third_party/pdfium"
os="$(uname -s)"
arch="$(uname -m)"

# plat = bblanchon platform tag; archive_libpath = path of the library inside
# the archive (mac/linux ship lib/libpdfium.*, windows ships bin/pdfium.dll).
case "$os" in
    Darwin)             plat="mac";   archive_libpath="lib/libpdfium.dylib" ;;
    Linux)              plat="linux"; archive_libpath="lib/libpdfium.so" ;;
    MINGW*|MSYS*|CYGWIN*) plat="win";  archive_libpath="bin/pdfium.dll" ;;
    *) echo "fetch-pdfium: unsupported OS '$os' — install PDFium manually and set EDOOKIT_PDFIUM_LIB" >&2; exit 1 ;;
esac
case "$arch" in
    arm64|aarch64) a="arm64" ;;
    x86_64|amd64)  a="x64" ;;
    *) echo "fetch-pdfium: unsupported arch '$arch'" >&2; exit 1 ;;
esac

# Canonical install location the runtime resolver checks (third_party/pdfium/lib).
# Windows ships the DLL in bin/, so we normalize it into lib/ after extraction.
case "$plat" in
    win) libpath="lib/pdfium.dll" ;;
    *)   libpath="$archive_libpath" ;;
esac

# SHA-256 of each pinned platform archive (pdfium-${plat}-${a}.tgz @ $PDFIUM_VERSION).
sha_for() {
    case "$1" in
        mac-arm64)   echo "935a50329d5f72466b2058f92f2c4a8f9e541abc8f3149b1994d078dec4190e1" ;;
        mac-x64)     echo "00de12ca1b9729119e7fd4901cee1f0e591367f20cf98e221a446bbf55c155d2" ;;
        linux-x64)   echo "6aeb4be0f790bf309c6b1e665552351845fe921a78d21697a1a9cb8ce427bb23" ;;
        linux-arm64) echo "26213696d0457ba07469cc23b8b112a2f0d316ceea0866a20b42d5216d603a93" ;;
        win-x64)     echo "d1a2b39c300f62daeec94f3a648a31d83d18605707bfdc5504d818d42cab13ce" ;;
        win-arm64)   echo "9f1b42f841d99dc1a592bf2c14419166476f4074321e5c6aef5ee92c217cdfaf" ;;
        *) return 1 ;;
    esac
}

libfile="$DIR/$libpath"
if [ -f "$libfile" ]; then
    echo "pdfium: present ($libfile)"
    exit 0
fi

# bblanchon publishes a .tgz per platform (including Windows).
asset="pdfium-${plat}-${a}.tgz"
want="$(sha_for "${plat}-${a}")" || {
    echo "fetch-pdfium: no pinned checksum for '${plat}-${a}'" >&2; exit 1
}
url="https://github.com/bblanchon/pdfium-binaries/releases/download/${PDFIUM_VERSION}/${asset}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "pdfium: downloading $asset ($PDFIUM_VERSION) ..."
curl -fsSL "$url" -o "$tmp/pdfium.tgz"

# Verify integrity before extracting anything.
if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$tmp/pdfium.tgz" | cut -d' ' -f1)"
else
    got="$(shasum -a 256 "$tmp/pdfium.tgz" | cut -d' ' -f1)"
fi
if [ "$got" != "$want" ]; then
    echo "fetch-pdfium: checksum mismatch for $asset @ $PDFIUM_VERSION" >&2
    echo "  expected $want" >&2
    echo "  got      $got" >&2
    exit 1
fi
echo "pdfium: checksum OK ($got)"

mkdir -p "$DIR"
tar -xzf "$tmp/pdfium.tgz" -C "$DIR"

# Normalize the library into lib/ (Windows ships it in bin/) so the runtime
# resolver finds it at a single canonical location across all platforms.
if [ ! -f "$libfile" ] && [ -f "$DIR/$archive_libpath" ]; then
    mkdir -p "$(dirname "$libfile")"
    cp "$DIR/$archive_libpath" "$libfile"
fi

if [ -f "$libfile" ]; then
    echo "pdfium: installed $libfile"
else
    echo "fetch-pdfium: archive extracted but $libfile not found; contents:" >&2
    find "$DIR" -name 'libpdfium*' -o -name 'pdfium.dll' >&2 || true
    exit 1
fi
