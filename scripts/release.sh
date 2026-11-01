#!/bin/sh
# Builds the release archives into build/release/ (run on macOS: the darwin builds need cgo for the menu
# bar icon; Linux, Windows and FreeBSD builds are pure Go and cross-compile from there, icon included
# except on FreeBSD, where the service runs without a tray icon).
#   scripts/release.sh [version]     default: git describe
# Each archive holds masamong, masamongd, the installer and the desktop files; SHA256SUMS lists them.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
version=${1:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}
out="$root/build/release"
rm -rf "$out"
mkdir -p "$out"
ldflags="-s -w -X masamong/internal/application.Version=$version"

build() { # os arch cgo   (arch armv7 = GOARCH=arm GOARM=7, e.g. 32-bit Raspberry Pi OS)
	name="masamong_$1_$2"
	goarch=$2 goarm=""
	[ "$2" = armv7 ] && goarch=arm goarm=7
	dir="$out/$name"
	ext=""
	[ "$1" = windows ] && ext=".exe"
	mkdir -p "$dir"
	for b in masamong masamongd; do
		(cd "$root" && CGO_ENABLED=$3 GOOS=$1 GOARCH=$goarch GOARM=$goarm go build -trimpath -ldflags "$ldflags" -o "$dir/$b$ext" "./cmd/$b")
	done
	if [ "$1" = windows ]; then
		cp "$root/install.ps1" "$dir/"
	else
		cp "$root/install.sh" "$dir/"
		mkdir -p "$dir/packaging/linux" "$dir/assets/brand/app"
		cp "$root/packaging/linux/masamong.desktop" "$dir/packaging/linux/"
		cp "$root/assets/brand/app/app-icon-linux-512.png" "$dir/assets/brand/app/"
	fi
	for notice in LICENSE THIRD_PARTY_NOTICES.md THIRD_PARTY_LICENSES.txt; do
		cp "$root/$notice" "$dir/"
	done
	printf 'Masamong %s (%s/%s)\n\nInstall: ./install.sh   (Windows: powershell -ExecutionPolicy Bypass -File install.ps1)\nThen:    masamong open\n\nBrand artwork: Created with Grok.\nLicenses: LICENSE, THIRD_PARTY_NOTICES.md, THIRD_PARTY_LICENSES.txt\n\nhttps://github.com/kim0040/masamong2\n' \
		"$version" "$1" "$2" >"$dir/README.txt"
	if [ "$1" = windows ]; then
		(cd "$out" && zip -qr "$name.zip" "$name")
	else
		(cd "$out" && COPYFILE_DISABLE=1 tar --no-xattrs --no-mac-metadata -czf "$name.tar.gz" "$name")
	fi
	rm -rf "$dir"
	echo "built $name"
}

if [ "$(uname -s)" = Darwin ]; then
	build darwin arm64 1
	build darwin amd64 1
else
	echo "skipping darwin (build the macOS archives on a Mac: the menu bar icon needs cgo)" >&2
fi
build linux amd64 0
build linux arm64 0
build linux armv7 0
build linux riscv64 0
build windows amd64 0
build windows arm64 0
build freebsd amd64 0
build freebsd arm64 0
(cd "$out" && shasum -a 256 masamong_* >SHA256SUMS)
echo "$out"
