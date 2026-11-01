#!/usr/bin/env python3
"""Verify and round-trip a source bundle into a PRIVATE SQLite staging store.

Staging stores each original typed row as JSON; it does not claim V2 schema parity.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import sqlite3

from snapshot_codec import decode, row_bytes


def verify(bundle, target):
    os.umask(0o077)
    manifest = json.loads((bundle / "manifest.json").read_text())
    if manifest["status"] != "complete":
        raise ValueError("Partial snapshot cannot be restored")
    if target.exists():
        raise ValueError("Target must be a new staging file")
    connection = sqlite3.connect(target)
    report = {"format": "masamong-staging-verification", "snapshot_tso": manifest["snapshot_tso"],
              "tables": [], "status": "incomplete", "v2_schema_migrated": False}
    try:
        connection.execute("CREATE TABLE source_schema (table_name TEXT PRIMARY KEY, metadata_json TEXT NOT NULL, ddl TEXT NOT NULL)")
        connection.execute("CREATE TABLE source_rows (table_name TEXT NOT NULL, ordinal INTEGER NOT NULL, row_json TEXT NOT NULL, PRIMARY KEY(table_name, ordinal)) WITHOUT ROWID")
        for table in manifest["tables"]:
            ddl_bytes = (bundle / table["ddl_file"]).read_bytes()
            if hashlib.sha256(ddl_bytes).hexdigest() != table["sha256_ddl"]:
                raise ValueError("DDL hash mismatch")
            name = table["table"]
            connection.execute("INSERT INTO source_schema VALUES (?,?,?)", (name, json.dumps(table), ddl_bytes.decode()))
            digest, count = hashlib.sha256(), 0
            with gzip.open(bundle / table["rows_file"], "rb") as stream:
                for payload in stream:
                    cells = json.loads(payload)
                    if len(cells) != len(table["columns"]):
                        raise ValueError("Column count mismatch")
                    if row_bytes([decode(cell) for cell in cells]) != payload:
                        raise ValueError("Type codec round-trip mismatch")
                    digest.update(payload); count += 1
                    connection.execute("INSERT INTO source_rows VALUES (?,?,?)", (name, count, payload.decode()))
            if count != table["source_count"] or digest.hexdigest() != table["sha256_uncompressed"]:
                raise ValueError("Export checksum/count mismatch")
            connection.commit()
            restored = hashlib.sha256()
            restored_count = 0
            for row, in connection.execute("SELECT row_json FROM source_rows WHERE table_name=? ORDER BY ordinal", (name,)):
                restored.update(row.encode()); restored_count += 1
            if restored.hexdigest() != digest.hexdigest() or restored_count != count:
                raise ValueError("SQLite staging round-trip mismatch")
            report["tables"].append({"table": name, "rows": count, "hash_match": True, "typed_roundtrip": True})
        integrity = connection.execute("PRAGMA integrity_check").fetchone()[0]
        if integrity != "ok":
            raise ValueError("SQLite integrity check failed")
        report.update(status="verified", total_rows=sum(t["rows"] for t in report["tables"]), integrity_check=integrity)
        (bundle / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps({"status": report["status"], "tables": len(report["tables"]), "rows": report["total_rows"], "integrity_check": integrity}))
    finally:
        connection.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bundle", type=Path)
    parser.add_argument("--staging", type=Path, required=True)
    args = parser.parse_args()
    verify(args.bundle, args.staging)
