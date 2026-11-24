# Common builds. The console (internal/transport/web/ui) is committed pre-built in ui/dist, so plain
# `go build` works without Node; run `make ui` after changing the console.
.PHONY: ui check build test parity docs docs-record diagrams codemap testnames testnames-update release-notes try install uninstall app release

ui:
	cd internal/transport/web/ui && npm ci && npm run build

check:
	cd internal/transport/web/ui && npm run check

build:
	go build -o build/masamong ./cmd/masamong
	go build -o build/masamongd ./cmd/masamongd

test:
	go test ./...

# the Python sandbox against the Go port on a made-up corpus (leaves, full-text ranking, retrieval incl. time-range
# and person cases); needs python3 with numpy, no stored chat or keys: PYTHON=python3.12 make parity
parity:
	scripts/parity.sh

# documentation: links resolve, translated pages are in step (docs/translations.json), and command names match the page's language
docs:
	go run ./tools/doccheck

# after updating every language of a changed page
docs-record:
	go run ./tools/doccheck -write

# diagrams: renders every docs/**/*.d2 to its light and dark SVG and stamps each with the hash of its
# source (make docs fails on a source changed without this). Needs d2; PNG previews: scripts/diagrams.sh --png DIR
diagrams:
	scripts/diagrams.sh

# code map: writes the generated indexes under docs/code-map (a stale map fails go test ./tools/codemap)
codemap:
	go run ./tools/codemap

# no test may disappear from internal/application/... when code moves between packages (golden: internal/application/testdata/testnames.golden)
testnames:
	scripts/testnames.sh

# after removing a test on purpose
testnames-update:
	scripts/testnames.sh -update

# draft the release notes of a version before tagging: make release-notes VERSION=v1.6.0 [SINCE=v1.5.1]
# (ANTHROPIC_API_KEY in the environment; without it a draft of commit subjects is written and the exit is non-zero).
# Read and edit release-notes/$(VERSION).json and .md, then commit them before the tag.
release-notes:
	$(if $(VERSION),,$(error usage: make release-notes VERSION=vX.Y.Z [SINCE=vA.B.C]))
	go run ./tools/relnotes --version $(VERSION) $(if $(SINCE),--since $(SINCE))

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
