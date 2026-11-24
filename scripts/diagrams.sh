#!/bin/sh
# Renders every D2 diagram under docs/ to two SVGs, light and dark: x.d2 -> x-light.svg, x-dark.svg.
#
#   scripts/diagrams.sh             render all (make diagrams)
#   scripts/diagrams.sh --png DIR   also write PNG previews into DIR, to look at; never commit them
#
# A source names only the classes of the shared look (entry, core, memory, external, store, ...); the
# look itself is _style-light.d2 / _style-dark.d2 beside the sources, put in front of each source here.
# Files whose name starts with "_" are not diagrams.
#
# Each SVG ends with two stamps: the sha256 of its source and of the style file it was rendered with
# (line endings normalised). tools/doccheck compares them with the files, so a source changed without a
# render fails `make docs`, and CI needs no d2.
#
# Needs d2 (https://d2lang.com) and, on the first run, curl. The text is set in Pretendard (SIL Open
# Font License 1.1), which has the Korean glyphs D2's own font lacks; d2 embeds only the glyphs a
# diagram uses. The font files are fetched once into a cache folder outside the repository and checked
# against the hashes below. MASAMONG_DIAGRAM_FONTS names another folder that already holds them.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
font_version=v1.3.9
font_base="https://github.com/orioncactus/pretendard/raw/$font_version/packages/pretendard/dist/public/static/alternative"
fonts=${MASAMONG_DIAGRAM_FONTS:-${XDG_CACHE_HOME:-$HOME/.cache}/masamong/pretendard-$font_version}
sum_regular=6d0af5258997aec7354a6e340fc2325ba321c410ca48b3af858c8c3d6e92a324
sum_semibold=5e1c548732af70873103066c16e1369b9a8a871f0b38c321a1d5bc73e43cea2d

png_dir=
if [ "${1:-}" = "--png" ]; then
	png_dir=${2:?--png needs a folder}
	mkdir -p "$png_dir"
fi

command -v d2 >/dev/null 2>&1 || {
	echo "diagrams: d2 is not installed (https://d2lang.com/tour/install)" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | cut -d' ' -f1
	else
		shasum -a 256 | cut -d' ' -f1
	fi
}

# the hash doccheck computes: the file's bytes without carriage returns
stamp_of() {
	tr -d '\r' <"$1" | sha256
}

font() {
	name=$1 want=$2
	path="$fonts/Pretendard-$name.ttf"
	if [ ! -f "$path" ]; then
		mkdir -p "$fonts"
		echo "diagrams: fetching Pretendard-$name.ttf ($font_version)" >&2
		curl -fsSL -o "$path.part" "$font_base/Pretendard-$name.ttf"
		mv "$path.part" "$path"
	fi
	got=$(sha256 <"$path")
	if [ "$got" != "$want" ]; then
		echo "diagrams: $path is not Pretendard $font_version (sha256 $got)" >&2
		exit 1
	fi
}
font Regular "$sum_regular"
font SemiBold "$sum_semibold"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

render() {
	# render <joined source> <output>: d2 picks the format from the output's extension
	d2 --font-regular "$fonts/Pretendard-Regular.ttf" --font-italic "$fonts/Pretendard-Regular.ttf" \
		--font-bold "$fonts/Pretendard-SemiBold.ttf" --font-semibold "$fonts/Pretendard-SemiBold.ttf" \
		"$1" "$2" >"$work/d2.log" 2>&1 || {
		cat "$work/d2.log" >&2
		exit 1
	}
}

count=0
for src in $(find "$root/docs" -name '*.d2' ! -name '_*' | sort); do
	dir=$(dirname "$src")
	name=$(basename "$src" .d2)
	for mode in light dark; do
		style="$dir/_style-$mode.d2"
		if [ ! -f "$style" ]; then
			echo "diagrams: $src has no $style" >&2
			exit 1
		fi
		joined="$work/$name-$mode.d2"
		cat "$style" "$src" >"$joined"
		out="$dir/$name-$mode.svg"
		render "$joined" "$work/$name-$mode.svg"
		{
			cat "$work/$name-$mode.svg"
			printf '\n<!-- d2-source-sha256: %s -->\n<!-- d2-style-sha256: %s -->\n' "$(stamp_of "$src")" "$(stamp_of "$style")"
		} >"$out"
		if [ -n "$png_dir" ]; then
			# a preview stands on the page colour GitHub would put behind the transparent SVG
			page="#ffffff"
			[ "$mode" = dark ] && page="#0d1117"
			{
				cat "$joined"
				printf '\nstyle.fill: "%s"\n' "$page"
			} >"$work/preview.d2"
			render "$work/preview.d2" "$png_dir/$name-$mode.png"
		fi
	done
	count=$((count + 1))
done
echo "diagrams: rendered $count diagram(s), light and dark"
