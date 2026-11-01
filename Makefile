# Common builds. The console (internal/transport/web/ui) is committed pre-built in ui/dist, so plain
# `go build` works without Node; run `make ui` after changing the console.
.PHONY: ui check build test docs docs-record try install uninstall app release

ui:
	cd internal/transport/web/ui && npm ci && npm run build

check:
	cd internal/transport/web/ui && npm run check

build:
	go build -o build/masamong ./cmd/masamong
	go build -o build/masamongd ./cmd/masamongd

test:
	go test ./...

# documentation: links resolve and translated pages are in step (docs/translations.json)
docs:
	go run ./tools/doccheck

# after updating every language of a changed page
docs-record:
	go run ./tools/doccheck -write

# build from source and run a separate test instance (~/.masamong-try), console opened; Ctrl+C stops it
ifeq ($(OS),Windows_NT)
try:
	powershell -NoProfile -ExecutionPolicy Bypass -File .\start-test.ps1 $(ARGS)
else
try:
	./start-test.command $(ARGS)
endif

# masamong + masamongd into ~/.local/bin (PREFIX=/opt/masamong make install for another place)
install:
	./install.sh $(if $(PREFIX),--prefix $(PREFIX))

uninstall:
	./install.sh --uninstall $(if $(PREFIX),--prefix $(PREFIX))

app:
	scripts/package-macos.sh build

# release archives for macOS, Linux and Windows into build/release (run on a Mac)
release:
	scripts/release.sh
