#!/usr/bin/env python3
"""Export only the explicitly selected database, never mutate the remote DB.

Requires existing python-dotenv and PyMySQL. No bot/config imports or model calls.
The private bundle is a source snapshot, not a V2 migration or SQL restore dump.
"""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import ssl
import sys
import time

from dotenv import dotenv_values
import pymysql
from snapshot_codec import row_bytes


def identifier(value):
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", value):
        raise ValueError("Unsupported SQL identifier")
    return "`" + value + "`"


def save(path, obj):
    path.write_text(json.dumps(obj, ensure_ascii=False, indent=2) + "\n")


def run(args):
    os.umask(0o077)
    env = dotenv_values(args.env, interpolate=False)
    needed = ["MASAMONG_DB_HOST", "MASAMONG_DB_PORT", "MASAMONG_DB_USER",
              "MASAMONG_DB_PASSWORD", "MASAMONG_DB_NAME"]
    if any(not env.get(k) or "${" in env[k] for k in needed):
        raise ValueError("Missing or unresolved DB configuration")
    if env.get("MASAMONG_DB_BACKEND") != "tidb" or env["MASAMONG_DB_NAME"] != args.expected_db:
        raise ValueError("Source backend/database does not match explicit selection")
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    manifest = {"format": "masamong-source-snapshot", "version": 1, "status": "incomplete",
                "captured_at": datetime.now(timezone.utc).isoformat(), "database": args.expected_db,
                "source_env_path": str(Path(args.env).resolve()), "tables": [],
                "limitations": ["V2 mapping not applied", "Not a SQL restore dump",
                                "No post-snapshot delta or sender state cutover",
                                "No GRANT/user/global configuration export"]}
    save(out / "manifest.json", manifest)
    ca = Path(env.get("MASAMONG_DB_SSL_CA") or "")
    context = ssl.create_default_context(cafile=str(ca) if ca.is_file() else None)
    manifest["tls_trust"] = "configured_ca" if ca.is_file() else "system_trust_store"
    manifest["tls_hostname_verified"] = context.check_hostname
    connection = None
    start = time.monotonic()
    try:
        connection = pymysql.connect(
            host=env["MASAMONG_DB_HOST"], port=int(env["MASAMONG_DB_PORT"]),
            user=env["MASAMONG_DB_USER"], password=env["MASAMONG_DB_PASSWORD"],
            database=args.expected_db, ssl=context, charset="utf8mb4",
            connect_timeout=10, read_timeout=45, write_timeout=10, autocommit=False)
        with connection.cursor() as cur:
            cur.execute("SELECT VERSION(), DATABASE(), @@session.time_zone")
            version, database, session_tz = cur.fetchone()
            if "TiDB" not in version or database != args.expected_db:
                raise ValueError("Connected server identity mismatch")
            manifest.update(server_version=version, session_time_zone=session_tz)
            cur.execute("SHOW SESSION STATUS LIKE 'Ssl_cipher'")
            manifest["tls_cipher"] = cur.fetchone()[1]
            if not manifest["tls_cipher"]:
                raise ValueError("TLS required")
            # Session-only resource limits, no global setting or operational config changes.
            cur.execute("SET SESSION tidb_distsql_scan_concurrency = 1")
            cur.execute("SET SESSION max_execution_time = 60000")
            cur.execute("START TRANSACTION READ ONLY AS OF TIMESTAMP NOW() - INTERVAL 5 SECOND")
            cur.execute("SELECT @@tidb_current_ts")
            manifest["snapshot_tso"] = str(cur.fetchone()[0])
            if manifest["snapshot_tso"] == "0":
                raise ValueError("Snapshot identity unavailable")
            cur.execute("SELECT TABLE_NAME, TABLE_TYPE FROM INFORMATION_SCHEMA.TABLES "
                        "WHERE TABLE_SCHEMA=DATABASE() ORDER BY TABLE_NAME")
            tables = cur.fetchall()
            if any(kind != "BASE TABLE" for _, kind in tables):
                raise ValueError("Views require a separate reviewed export contract")
        for table, _ in tables:
            if time.monotonic() - start > args.max_seconds:
                raise TimeoutError("Snapshot duration budget reached; discard partial run")
            quoted = identifier(table)
            with connection.cursor() as cur:
                cur.execute("SHOW CREATE TABLE " + quoted)
                ddl = cur.fetchone()[1]
                (out / (table + ".schema.sql")).write_text(ddl + ";\n")
                cur.execute("SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, "
                            "EXTRA, ORDINAL_POSITION FROM INFORMATION_SCHEMA.COLUMNS "
                            "WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=%s ORDER BY ORDINAL_POSITION", (table,))
                columns = [{"name": c[0], "sql_type": c[1], "nullable": c[2],
                            "default": c[3], "extra": c[4], "ordinal": c[5]} for c in cur.fetchall()]
                cur.execute("SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS "
                            "WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=%s AND INDEX_NAME='PRIMARY' "
                            "ORDER BY SEQ_IN_INDEX", (table,))
                primary_key = [r[0] for r in cur.fetchall()]
                cur.execute("SELECT COUNT(*) FROM " + quoted)
                expected = cur.fetchone()[0]
            projection = ",".join(identifier(c["name"]) for c in columns)
            order = " ORDER BY " + ",".join(map(identifier, primary_key)) if primary_key else ""
            file = out / (table + ".rows.jsonl.gz")
            digest = hashlib.sha256()
            rows, uncompressed = 0, 0
            with connection.cursor(pymysql.cursors.SSCursor) as cur, gzip.open(file, "wb", compresslevel=3) as stream:
                cur.execute("SELECT " + projection + " FROM " + quoted + order)
                for row in cur:
                    payload = row_bytes(row)
                    stream.write(payload); digest.update(payload)
                    rows += 1; uncompressed += len(payload)
                    if rows % 2000 == 0 and time.monotonic() - start > args.max_seconds:
                        raise TimeoutError("Snapshot duration budget reached")
            if rows != expected:
                raise ValueError("Snapshot count mismatch")
            entry = {"table": table, "columns": columns, "primary_key": primary_key,
                     "source_count": expected, "exported_count": rows,
                     "rows_file": file.name, "sha256_uncompressed": digest.hexdigest(),
                     "uncompressed_bytes": uncompressed, "compressed_bytes": file.stat().st_size,
                     "ddl_file": table + ".schema.sql",
                     "sha256_ddl": hashlib.sha256((out / (table + ".schema.sql")).read_bytes()).hexdigest()}
            manifest["tables"].append(entry)
            save(out / "manifest.json", manifest)
            print(json.dumps({"table": table, "rows": rows, "compressed_bytes": file.stat().st_size}), flush=True)
        with connection.cursor() as cur:
            cur.execute("ROLLBACK")
        manifest.update(status="complete", completed_at=datetime.now(timezone.utc).isoformat(),
                        elapsed_seconds=round(time.monotonic()-start, 3),
                        total_rows=sum(t["exported_count"] for t in manifest["tables"]))
        save(out / "manifest.json", manifest)
        print(json.dumps({"status": "complete", "tables": len(tables), "rows": manifest["total_rows"]}), flush=True)
    except Exception as exc:
        manifest["failure"] = {"type": type(exc).__name__,
                               "code": exc.args[0] if exc.args and isinstance(exc.args[0], int) else None}
        save(out / "manifest.json", manifest)
        raise
    finally:
        if connection is not None:
            connection.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env", required=True)
    parser.add_argument("--expected-db", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--max-seconds", type=int, default=240)
    options = parser.parse_args()
    try:
        run(options)
    except Exception as exc:
        print(json.dumps({"status": "failed", "error_type": type(exc).__name__,
                          "code": exc.args[0] if exc.args and isinstance(exc.args[0], int) else None}), file=sys.stderr)
        sys.exit(1)
