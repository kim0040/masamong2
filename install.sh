#!/bin/sh
# Installs (or updates) the masamong CLI and the masamongd service for the current user — no root.
#
#   ./install.sh                  from a clone: builds with Go; from a release archive: copies the binaries
#   ./install.sh --prefix DIR     install into DIR/bin (default: ~/.local)
#   ./install.sh --no-modify-path do not add the bin folder to the shell profile
#   ./install.sh --app            macOS: also build ~/Applications/Masamong.app (double-click to open)
#   ./install.sh --uninstall      stop the service, turn off start at login, remove the binaries (data stays)
#
# Without Go and without prebuilt binaries next to this script it downloads the matching archive from
# the GitHub release (MASAMONG_VERSION=v1.0.0 picks one; default: latest) and checks it against SHA256SUMS.
# Piped from the web (curl -fsSL …/install.sh | sh, options after `sh -s --`) it always downloads.
set -eu

# Run from a file, the script's folder may hold a release archive or a clone. Piped into a shell, $0 is the
# shell itself: there is no folder of ours, so look for nothing next to it.
here=""
case "$(basename -- "$0")" in
sh | bash | dash | ash | ksh | zsh | -sh | -bash) ;;
*) [ -f "$0" ] && here=$(cd "$(dirname "$0")" && pwd) ;;
esac

REPO="kim0040/masamong2"
prefix="${PREFIX:-$HOME/.local}"
modify_path=1
want_app=0
uninstall=0
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix) prefix="$2"; shift ;;
	--prefix=*) prefix="${1#--prefix=}" ;;
	--no-modify-path) modify_path=0 ;;
	--app) want_app=1 ;;
	--uninstall) uninstall=1 ;;
	-h | --help)
		if [ -n "$here" ]; then sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; else echo "usage: see https://github.com/kim0040/masamong2/blob/main/install.sh"; fi
		exit 0 ;;
	*) echo "install.sh: unknown option $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

bindir="$prefix/bin"
docdir="$prefix/share/doc/masamong"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
armv7l | armv7 | armv8l) arch=armv7 ;;
riscv64) arch=riscv64 ;;
*) die "unsupported CPU $arch (supported: amd64, arm64, armv7, riscv64)" ;;
esac
case "$os" in
darwin | linux | freebsd) ;;
*) die "unsupported system $os; on Windows use install.ps1" ;;
esac

if [ "$uninstall" = 1 ]; then
	if [ -x "$bindir/masamong" ]; then
		"$bindir/masamong" autostart off --output json >/dev/null 2>&1 || true
		"$bindir/masamong" stop --output json >/dev/null 2>&1 || true
	fi
	rm -f "$bindir/masamong" "$bindir/masamongd"
	rm -rf "$docdir"
	rm -f "$HOME/.local/share/applications/masamong.desktop" "$HOME/.local/share/icons/hicolor/512x512/apps/masamong.png"
	rm -rf "$HOME/Applications/Masamong.app"
	say "removed masamong from $bindir. Your data folder was kept (masamong status showed where)."
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# 1) a release archive ships the binaries next to this script
if [ -n "$here" ] && [ -x "$here/masamong" ] && [ -x "$here/masamongd" ] && [ ! -f "$here/go.mod" ]; then
	say "installing the prebuilt binaries from $here"
	cp "$here/masamong" "$here/masamongd" "$tmp/"
# 2) a source checkout builds with Go
elif [ -n "$here" ] && [ -f "$here/go.mod" ] && command -v go >/dev/null 2>&1; then
	need=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' "$here/go.mod")
	have=$(go env GOVERSION | sed 's/^go//')
	if [ "$(printf '%s\n%s\n' "$need" "$have" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)" != "$need" ]; then
		die "Go $need or newer is needed (found $have). Update Go (https://go.dev/dl/) or use a release archive."
	fi
	version=$(git -C "$here" describe --tags --always --dirty 2>/dev/null || echo dev)
	ldflags="-s -w -X masamong/internal/application.Version=$version"
	cgo=0
	# the macOS menu bar icon needs cgo (Xcode command line tools); without them the build has no icon
	if [ "$os" = darwin ] && xcrun --find clang >/dev/null 2>&1; then cgo=1; fi
	if [ "$os" = darwin ] && [ "$cgo" = 0 ]; then
		say "note: no Xcode command line tools (xcode-select --install); building without the menu bar icon"
	fi
	say "building masamong $version with $(go env GOVERSION)…"
	(cd "$here" && CGO_ENABLED=$cgo go build -trimpath -ldflags "$ldflags" -o "$tmp/masamong" ./cmd/masamong) || die "go build failed (see above)"
	(cd "$here" && CGO_ENABLED=$cgo go build -trimpath -ldflags "$ldflags" -o "$tmp/masamongd" ./cmd/masamongd) || die "go build failed (see above)"
# 3) otherwise download the release for this computer
else
	command -v curl >/dev/null 2>&1 || die "neither Go (to build) nor curl (to download a release) is available"
	tag="${MASAMONG_VERSION:-latest}"
	name="masamong_${os}_${arch}.tar.gz"
	if [ "$tag" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$tag"
	fi
	say "downloading $base/$name"
	curl -fsSL "$base/$name" -o "$tmp/$name" || die "download failed. Install Go and run this script from a clone, or download a release by hand from https://github.com/$REPO/releases"
	# the same check install.ps1 makes: the archive must match the release's SHA256SUMS
	curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "could not download SHA256SUMS for the release"
	want=$(awk -v n="$name" '$2 == n || $2 == "*" n { print tolower($1); exit }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS has no checksum for $name"
	if command -v sha256sum >/dev/null 2>&1; then
		got=$(sha256sum "$tmp/$name" | awk '{ print tolower($1) }')
	elif command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "$tmp/$name" | awk '{ print tolower($1) }')
	elif command -v sha256 >/dev/null 2>&1; then
		got=$(sha256 -q "$tmp/$name" | tr '[:upper:]' '[:lower:]')
	else
		die "no SHA-256 tool (sha256sum, shasum or sha256) to check the download"
	fi
	[ "$got" = "$want" ] || die "checksum mismatch for $name: the download is damaged or not the published file"
	say "checksum ok ($name)"
	tar -xzf "$tmp/$name" -C "$tmp" || die "the archive is damaged"
	for b in masamong masamongd; do
		found=$(find "$tmp" -type f -name "$b" | head -1)
		[ -n "$found" ] || die "the archive has no $b"
		[ "$found" = "$tmp/$b" ] || mv "$found" "$tmp/$b"
	done
fi

"$tmp/masamong" version --output json >/dev/null 2>&1 || die "the new masamong does not run on this computer"

# an update keeps the running service alive: new files replace the old ones by rename, then it restarts
running=0
if [ -x "$bindir/masamong" ] && "$bindir/masamong" status --output json 2>/dev/null | grep -q '"running":true'; then
	running=1
fi
mkdir -p "$bindir" || die "cannot create $bindir"
for b in masamong masamongd; do
	chmod 755 "$tmp/$b"
	cp "$tmp/$b" "$bindir/.$b.new" || die "cannot write to $bindir"
	mv -f "$bindir/.$b.new" "$bindir/$b"
done
if [ "$os" = darwin ]; then
	# downloaded files carry a quarantine flag that makes macOS refuse to run them
	xattr -d com.apple.quarantine "$bindir/masamong" "$bindir/masamongd" 2>/dev/null || true
fi
say "installed $bindir/masamong and $bindir/masamongd"

# Keep the license terms beside user installations. A downloaded archive is
# unpacked under $tmp, while source and manually extracted archives use $here.
mkdir -p "$docdir"
for notice in LICENSE THIRD_PARTY_NOTICES.md THIRD_PARTY_LICENSES.txt; do
	source_file="$here/$notice"
	if [ ! -f "$source_file" ]; then
		source_file=$(find "$tmp" -type f -name "$notice" | head -1)
	fi
	[ -n "$source_file" ] && [ -f "$source_file" ] && cp "$source_file" "$docdir/$notice"
done

if [ "$running" = 1 ]; then
	"$bindir/masamong" restart --output json >/dev/null 2>&1 && say "restarted the running service with the new version" ||
		say "the service is still running the old version: run 'masamong restart'"
fi

# desktop entries: a Linux desktop gets a launcher, macOS can get Masamong.app
if [ "$os" = linux ] && { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; } && [ -f "$here/packaging/linux/masamong.desktop" ]; then
	mkdir -p "$HOME/.local/share/applications" "$HOME/.local/share/icons/hicolor/512x512/apps"
	sed "s|^Exec=masamong|Exec=$bindir/masamong|" "$here/packaging/linux/masamong.desktop" >"$HOME/.local/share/applications/masamong.desktop"
	[ -f "$here/assets/brand/app/app-icon-linux-512.png" ] && cp "$here/assets/brand/app/app-icon-linux-512.png" "$HOME/.local/share/icons/hicolor/512x512/apps/masamong.png"
	say "added Masamong to the applications menu"
fi
if [ "$want_app" = 1 ]; then
	[ "$os" = darwin ] || die "--app is for macOS"
	[ -x "$here/scripts/package-macos.sh" ] || die "--app needs a source checkout"
	mkdir -p "$HOME/Applications"
	"$here/scripts/package-macos.sh" "$HOME/Applications" >/dev/null && say "built $HOME/Applications/Masamong.app"
fi

on_path=0
case ":$PATH:" in *":$bindir:"*) on_path=1 ;; esac
if [ "$on_path" = 0 ]; then
	line="export PATH=\"$bindir:\$PATH\""
	case "${SHELL:-}" in
	*/zsh) rc="$HOME/.zshrc" ;;
	*/bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
	*/fish) rc="" ; line="fish_add_path $bindir" ;;
	*) rc="$HOME/.profile" ;;
	esac
	if [ "$modify_path" = 1 ] && [ -n "$rc" ]; then
		if ! grep -qs "# added by masamong install.sh" "$rc"; then
			printf '\n%s  # added by masamong install.sh\n' "$line" >>"$rc"
		fi
		say "added $bindir to PATH in $rc — open a new terminal (or run: $line)"
	else
		say "$bindir is not on your PATH. Add it with: $line"
	fi
fi

cat <<EOF

Next:
  masamong open               start the service and open the console in your browser
  masamong start              start it without a browser (servers)
  masamong connect user@host  manage masamong on a server from this computer's browser
  masamong status | stop | restart | autostart on
EOF
