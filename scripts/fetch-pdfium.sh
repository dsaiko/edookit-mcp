#!/usr/bin/env bash
# Fetches the prebuilt PDFium shared library for this host from
# bblanchon/pdfium-binaries into third_party/pdfium/, so attachment
# rasterization works out of the box (the app loads it at runtime — see
# src/tools/pdfrender.rs). Idempotent: skips the download if the lib is present.
set -euo pipefail

DIR="third_party/pdfium"
os="$(uname -s)"
arch="$(uname -m)"

# plat = bblanchon platform tag; libpath = relative path of the library inside
# the archive (mac/linux ship lib/libpdfium.*, windows ships bin/pdfium.dll).
case "$os" in
    Darwin)             plat="mac";   libpath="lib/libpdfium.dylib" ;;
    Linux)              plat="linux"; libpath="lib/libpdfium.so" ;;
    MINGW*|MSYS*|CYGWIN*) plat="win";  libpath="bin/pdfium.dll" ;;
    *) echo "fetch-pdfium: unsupported OS '$os' — install PDFium manually and set EDOOKIT_PDFIUM_LIB" >&2; exit 1 ;;
esac
case "$arch" in
    arm64|aarch64) a="arm64" ;;
    x86_64|amd64)  a="x64" ;;
    *) echo "fetch-pdfium: unsupported arch '$arch'" >&2; exit 1 ;;
esac

libfile="$DIR/$libpath"
if [ -f "$libfile" ]; then
    echo "pdfium: present ($libfile)"
    exit 0
fi

# bblanchon publishes a .tgz per platform (including Windows).
asset="pdfium-${plat}-${a}.tgz"
url="https://github.com/bblanchon/pdfium-binaries/releases/latest/download/${asset}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "pdfium: downloading $asset ..."
curl -fsSL "$url" -o "$tmp/pdfium.tgz"
mkdir -p "$DIR"
tar -xzf "$tmp/pdfium.tgz" -C "$DIR"

if [ -f "$libfile" ]; then
    echo "pdfium: installed $libfile"
else
    echo "fetch-pdfium: archive extracted but $libfile not found; contents:" >&2
    find "$DIR" -name 'libpdfium*' -o -name 'pdfium.dll' >&2 || true
    exit 1
fi
