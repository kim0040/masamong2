#!/bin/sh
# Checks the archives scripts/release.sh wrote (CI runs this right after the build):
#   scripts/verify-release.sh [dir]     default: build/release
# Every target is present, SHA256SUMS matches, and each archive carries both binaries, its installer and
# the license files unchanged; masamongd must embed the Pretendard OFL text next to the console fonts.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
dir=${1:-"$root/build/release"}
dir=$(cd "$dir" && pwd)
targets="darwin_arm64 darwin_amd64 linux_amd64 linux_arm64 linux_armv7 linux_riscv64 windows_amd64 windows_arm64 freebsd_amd64 freebsd_arm64"
count=$(echo $targets | wc -w | tr -d ' ')
notices="LICENSE THIRD_PARTY_NOTICES.md THIRD_PARTY_LICENSES.txt"
font_marker="with Reserved Font Name Pretendard"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
fail() { printf 'verify-release: %s\n' "$*" >&2; exit 1; }

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
[ -f "$dir/SHA256SUMS" ] || fail "SHA256SUMS is missing"
(cd "$dir" && sha -c SHA256SUMS) || fail "SHA256SUMS does not match the archives"
[ "$(wc -l <"$dir/SHA256SUMS" | tr -d ' ')" = "$count" ] || fail "SHA256SUMS should list $count archives"

for t in $targets; do
	name="masamong_$t"
	out="$tmp/$t"
	mkdir -p "$out"
	case "$t" in
	windows_*)
		archive="$name.zip" ext=".exe" installer="install.ps1"
		[ -f "$dir/$archive" ] || fail "$archive is missing"
		unzip -q "$dir/$archive" -d "$out"
		;;
	*)
		archive="$name.tar.gz" ext="" installer="install.sh"
		[ -f "$dir/$archive" ] || fail "$archive is missing"
		tar -xzf "$dir/$archive" -C "$out"
		;;
	esac
	grep -q " $archive\$" "$dir/SHA256SUMS" || fail "SHA256SUMS does not list $archive"
	for f in "masamong$ext" "masamongd$ext" "$installer" README.txt $notices; do
		[ -f "$out/$name/$f" ] || fail "$archive has no $name/$f"
	done
	for f in $notices; do
		cmp -s "$root/$f" "$out/$name/$f" || fail "$archive: $f differs from the repository copy"
	done
	grep -aqF "$font_marker" "$out/$name/masamongd$ext" || fail "$archive: masamongd does not embed LICENSE-PRETENDARD.txt"
	echo "ok $archive"
done
echo "verified $count archives and SHA256SUMS in $dir"
