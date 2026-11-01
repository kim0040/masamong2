#!/usr/bin/env python3
"""Audit shipped dependencies and maintain the distributable license bundle.

The audit intentionally follows the two release binaries rather than treating
every module in go.sum as shipped code. Frontend checks cover every package in
package-lock.json; complete license texts are bundled for code/assets that are
present in the compiled console.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]
UI = ROOT / "internal/transport/web/ui"
NOTICE_BUNDLE = ROOT / "THIRD_PARTY_LICENSES.txt"

ALLOWED_NPM_LICENSES = {
    "Apache-2.0",
    "BSD-3-Clause",
    "ISC",
    "MIT",
    "MPL-2.0",
    "OFL-1.1",
}

# Keep in step with the build calls at the end of scripts/release.sh.
# Every archive scripts/release.sh builds ("arm" is its armv7 build, GOARM=7).
RELEASE_TARGETS = (
    ("darwin", "arm64", "1"),
    ("darwin", "amd64", "1"),
    ("linux", "amd64", "0"),
    ("linux", "arm64", "0"),
    ("linux", "arm", "0"),
    ("linux", "riscv64", "0"),
    ("windows", "amd64", "0"),
    ("windows", "arm64", "0"),
    ("freebsd", "amd64", "0"),
    ("freebsd", "arm64", "0"),
)

UI_RUNTIME_LICENSES = (
    ("Svelte", "package-lock.json version", UI / "node_modules/svelte/LICENSE.md"),
    ("Lucide", "package-lock.json version", UI / "node_modules/@lucide/svelte/LICENSE"),
    ("Pretendard", "1.3.9", UI / "node_modules/pretendard/dist/LICENSE.txt"),
)


def fail(message: str) -> None:
    print(f"license audit: {message}", file=sys.stderr)
    raise SystemExit(1)


def go_dependencies() -> list[tuple[str, str, Path]]:
    template = (
        "{{if .Module}}{{if not .Module.Main}}"
        "{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}"
        "{{end}}{{end}}"
    )
    command = [
        "go",
        "list",
        "-deps",
        "-f",
        template,
        "./cmd/masamong",
        "./cmd/masamongd",
    ]
    # The module set differs per platform (terminal and libc helpers), so the
    # bundle covers every archive scripts/release.sh ships, whatever the host.
    rows: set[tuple[str, str, Path]] = set()
    for goos, goarch, cgo in RELEASE_TARGETS:
        env = os.environ.copy()
        env.setdefault(
            "GOCACHE", str(Path(tempfile.gettempdir()) / "masamong-license-go-build")
        )
        env.update(GOOS=goos, GOARCH=goarch, CGO_ENABLED=cgo, GOARM="7")
        try:
            output = subprocess.check_output(command, cwd=ROOT, env=env, text=True)
        except (OSError, subprocess.CalledProcessError) as exc:
            fail(f"cannot inspect Go dependencies for {goos}/{goarch}: {exc}")
        for line in output.splitlines():
            if not line:
                continue
            module, version, directory = line.split("|", 2)
            rows.add((module, version, Path(directory)))
    return sorted(rows, key=lambda row: (row[0], row[1]))


def license_files(directory: Path) -> list[Path]:
    prefixes = ("LICENSE", "LICENCE", "COPYING", "NOTICE")
    return sorted(
        path
        for path in directory.iterdir()
        if path.is_file() and path.name.upper().startswith(prefixes)
    )


def audit_go() -> list[tuple[str, str, Path, list[Path]]]:
    dependencies = []
    for module, version, directory in go_dependencies():
        if not directory.is_dir():
            fail(f"Go module directory is missing: {module} {version}")
        files = license_files(directory)
        if not files:
            fail(f"Go module has no root license or notice: {module} {version}")
        primary_path = next(
            (path for path in files if path.name.upper() in {"LICENSE", "LICENSE.TXT", "LICENCE", "COPYING"}),
            files[0],
        )
        primary = primary_path.read_text(errors="replace")
        upper = primary.upper()
        accepted_markers = (
            "MOZILLA PUBLIC LICENSE VERSION 2.0",
            "APACHE LICENSE",
            "MIT LICENSE",
            "PERMISSION IS HEREBY GRANTED, FREE OF CHARGE",
            "REDISTRIBUTION AND USE IN SOURCE AND BINARY FORMS",
            "THIS PROJECT IS COVERED BY TWO DIFFERENT LICENSES: MIT AND APACHE",
            "SQLITE IS PUBLIC DOMAIN",
        )
        if not any(marker in upper for marker in accepted_markers):
            fail(
                f"unreviewed primary license text in shipped Go module: "
                f"{module} {version} ({primary_path.name})"
            )
        dependencies.append((module, version, directory, files))
    return dependencies


def frontend_packages() -> tuple[dict[str, int], dict[str, str]]:
    lock_path = UI / "package-lock.json"
    data = json.loads(lock_path.read_text())
    counts: dict[str, int] = {}
    versions: dict[str, str] = {}
    for package_path, package in data.get("packages", {}).items():
        if not package_path:
            continue
        license_name = package.get("license")
        if not license_name:
            fail(f"package-lock entry has no license: {package_path}")
        if license_name not in ALLOWED_NPM_LICENSES:
            fail(f"unreviewed npm license {license_name!r}: {package_path}")
        counts[license_name] = counts.get(license_name, 0) + 1
        name = package_path.removeprefix("node_modules/")
        versions[name] = package.get("version", "unknown")
    return dict(sorted(counts.items())), versions


def build_bundle(
    go_modules: list[tuple[str, str, Path, list[Path]]],
    frontend_versions: dict[str, str],
) -> str:
    lines = [
        "MASAMONG THIRD-PARTY LICENSES",
        "================================",
        "",
        "This file contains license and notice texts for third-party software",
        "distributed in the Masamong release binaries and embedded web console.",
        "Masamong's own source code and documentation are covered by LICENSE.",
        "",
    ]

    for module, version, _directory, files in go_modules:
        for path in files:
            lines.extend(
                [
                    "-" * 78,
                    f"Go module: {module} {version}",
                    f"Upstream file: {path.name}",
                    "-" * 78,
                    "",
                    path.read_text(errors="replace").rstrip(),
                    "",
                ]
            )

    for display_name, declared_version, path in UI_RUNTIME_LICENSES:
        if not path.is_file():
            fail(
                f"{display_name} license is missing at {path.relative_to(ROOT)}; "
                "run npm ci in internal/transport/web/ui"
            )
        package_name = "@lucide/svelte" if display_name == "Lucide" else display_name.lower()
        version = frontend_versions.get(package_name, declared_version)
        lines.extend(
            [
                "-" * 78,
                f"Embedded web asset/runtime: {display_name} {version}",
                f"Upstream file: {path.name}",
                "-" * 78,
                "",
                path.read_text(errors="replace").rstrip(),
                "",
            ]
        )

    return "\n".join(lines).rstrip() + "\n"


def main() -> None:
    parser = argparse.ArgumentParser(
        description="audit release dependency licenses and check the bundled notices"
    )
    parser.add_argument(
        "--write",
        action="store_true",
        help="regenerate THIRD_PARTY_LICENSES.txt after a reviewed dependency change",
    )
    args = parser.parse_args()

    project_license = (ROOT / "LICENSE").read_text(errors="replace")
    if not project_license.startswith("MIT License\n"):
        fail("root LICENSE is missing or is not the expected MIT text")

    go_modules = audit_go()
    npm_counts, frontend_versions = frontend_packages()
    font_license = UI / "public/fonts/LICENSE-PRETENDARD.txt"
    upstream_font_license = UI / "node_modules/pretendard/dist/LICENSE.txt"
    if not font_license.is_file() or font_license.read_bytes() != upstream_font_license.read_bytes():
        fail("the distributed Pretendard OFL text is missing or differs from the package")
    built_font_license = UI / "dist/fonts/LICENSE-PRETENDARD.txt"
    if not built_font_license.is_file() or built_font_license.read_bytes() != font_license.read_bytes():
        fail("ui/dist lacks the Pretendard OFL text embedded in the binaries; run make ui")
    expected = build_bundle(go_modules, frontend_versions)

    if args.write:
        NOTICE_BUNDLE.write_text(expected)
        print(f"wrote {NOTICE_BUNDLE.relative_to(ROOT)}")
    elif not NOTICE_BUNDLE.is_file() or NOTICE_BUNDLE.read_text() != expected:
        fail("THIRD_PARTY_LICENSES.txt is stale; review dependencies, then run with --write")

    print(f"Go release modules: {len(go_modules)} (all have license/notice files)")
    print(
        "Frontend lock packages: "
        f"{sum(npm_counts.values())} "
        + ", ".join(f"{name}={count}" for name, count in npm_counts.items())
    )
    print("PASS: no unknown, proprietary, noncommercial, GPL, or AGPL release license")


if __name__ == "__main__":
    main()
