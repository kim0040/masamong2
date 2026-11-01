#!/bin/sh
# Builds Masamong.app: double-clicking it starts (or reuses) the service and opens the console in the
# default browser, signed in. The bot keeps running after the browser closes.
#   scripts/package-macos.sh [output-dir]      (default: build/)
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-"$root/build"}
app="$out/Masamong.app"
version=$(sed -n 's/^var Version = "\(.*\)"/\1/p' "$root/internal/application/service.go")
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
(cd "$root" && go build -trimpath -ldflags "-s -w" -o "$app/Contents/MacOS/masamong" ./cmd/masamong)
(cd "$root" && go build -trimpath -ldflags "-s -w" -o "$app/Contents/MacOS/masamongd" ./cmd/masamongd)
cp "$root/assets/brand/app/app-icon.icns" "$app/Contents/Resources/app-icon.icns"
mkdir -p "$app/Contents/Resources/Licenses"
for notice in LICENSE THIRD_PARTY_NOTICES.md THIRD_PARTY_LICENSES.txt; do
	cp "$root/$notice" "$app/Contents/Resources/Licenses/"
done
# the launcher script must not be called "Masamong": macOS file names are case-insensitive and it would
# replace the masamong binary.
cat > "$app/Contents/MacOS/launch" <<'SH'
#!/bin/sh
# Finder starts apps without a terminal: the launcher's errors go to the data folder's log.
here=$(cd "$(dirname "$0")" && pwd)
exec "$here/masamong" open --output json >/dev/null 2>>"${TMPDIR:-/tmp}/masamong-launcher.log"
SH
chmod +x "$app/Contents/MacOS/launch"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Masamong</string>
  <key>CFBundleDisplayName</key><string>마사몽</string>
  <key>CFBundleIdentifier</key><string>app.masamong.launcher</string>
  <key>CFBundleExecutable</key><string>launch</string>
  <key>CFBundleIconFile</key><string>app-icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
echo "$app"
