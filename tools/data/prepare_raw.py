#!/usr/bin/env python3
"""Offline preparation only: Discord raw tables + Kakao CSVs, no legacy derivatives.

Creates a new private source corpus, not V2 embeddings/claims or a live importer.
"""
import argparse
import csv
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3


RAW_TABLES = ("conversation_history", "conversation_history_archive")
RAW_FIELDS = ("message_id", "guild_id", "channel_id", "user_id", "user_name", "content", "is_bot", "created_at")


def canonical(value):
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":"))


def file_hash(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def identifier(value):
    return None if value is None else str(value)


def timestamp(value):
    if not value:
        return {"raw": value, "parsed": None, "timezone": "unknown", "status": "missing"}, None
    if not isinstance(value, str):
        raise ValueError("Unexpected timestamp SQL type")
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        return {"raw": value, "parsed": None, "timezone": "unknown", "status": "invalid"}, None
    aware = parsed.utcoffset() is not None
    sort = parsed.astimezone(timezone.utc).isoformat() if aware else parsed.isoformat()
    return {"raw": value, "parsed": parsed.isoformat(),
            "timezone": "explicit_offset" if aware else "unknown", "status": "parsed"}, sort


def observation(platform, collection, room, source, ordinal, native_id, actor, body, time_raw, bot=None):
    if body is not None and not isinstance(body, str):
        raise ValueError("Unexpected raw body SQL type")
    time_info, sort = timestamp(time_raw)
    # Identical bytes from different rooms are separate source observations.
    source_key = canonical([platform, room, source.get("kind"), source.get("table"),
                            source["fingerprint"], ordinal])
    obs_id = hashlib.sha256(source_key.encode()).hexdigest()
    record = {"format": "masamong-raw-observation/v1", "observation_id": obs_id,
              "platform": platform, "collection_id": collection, "source_room_id": room,
              "native_message_id": identifier(native_id), "source": {**source, "record_ordinal": ordinal},
              "actor": actor, "content": body, "time": time_info,
              "is_bot": bot, "record_kind": "unclassified_observation",
              "access": {"status": "source_policy_unresolved", "external_share_granted": False},
              "field_availability": {"native_reply": "not_available_in_source",
                                     "edit_history": "not_available_in_source",
                                     "attachment_body": "not_available_in_source"}}
    return record, sort


def discord_record(row, table, fingerprint, ordinal, tso):
    selected = {name: row[name] for name in RAW_FIELDS}
    for field in ("message_id", "guild_id", "channel_id", "user_id"):
        selected[field] = identifier(selected[field])
    actor = {"native_account_id": selected["user_id"], "observed_label": selected["user_name"], "person_id": None}
    guild = selected["guild_id"]
    room = "discord:" + (guild or "unknown-guild") + ":" + (selected["channel_id"] or "unknown-channel")
    rec, sort = observation("discord", "discord", room,
                            {"kind": "tidb_raw_table", "table": table, "snapshot_tso": tso,
                             "fingerprint": fingerprint}, ordinal, selected["message_id"], actor,
                            selected["content"], selected["created_at"], selected["is_bot"])
    rec["raw_fields"] = selected
    rec["source_guild_id"] = guild
    rec["source_channel_id"] = selected["channel_id"]
    rec["record_kind"] = "bot_record" if selected["is_bot"] == 1 else "unclassified_observation"
    return rec, sort


def kakao_record(row, config, fingerprint, ordinal):
    if set(row) != {"Date", "User", "Message"} or any(v is None for v in row.values()):
        raise ValueError("Kakao CSV must have exactly three valid fields")
    actor = {"native_account_id": None, "observed_label": row["User"], "person_id": None}
    rec, sort = observation("kakao", config["collection_id"], config["source_room_id"],
                            {"kind": "kakao_csv", "file": config["path"], "fingerprint": fingerprint},
                            ordinal, None, actor, row["Message"], row["Date"])
    rec["raw_fields"] = row
    if not row["Date"] and not row["User"]:
        rec["record_kind"] = "unclassified_marker"
    return rec, sort


def safe_bundle_path(bundle, filename):
    path = (bundle / filename).resolve()
    if path.parent != bundle.resolve() or path.is_symlink() or not path.is_file():
        raise ValueError("Snapshot input must be a direct local file")
    return path


def run(bundle, config_file, project, out):
    os.umask(0o077)
    out.mkdir(parents=True, mode=0o700, exist_ok=False)
    config = json.loads(config_file.read_text())
    source_manifest = json.loads((bundle / "manifest.json").read_text())
    if source_manifest.get("status") != "complete":
        raise ValueError("Source snapshot is incomplete")
    tables = {t["table"]: t for t in source_manifest["tables"]}
    report = {"format": "masamong-raw-corpus/v1", "status": "incomplete",
              "created_at": datetime.now(timezone.utc).isoformat(),
              "policy": "raw_only_rebuild", "source_snapshot_tso": source_manifest["snapshot_tso"],
              "allowed_discord_tables": list(RAW_TABLES), "legacy_derivatives_used": False,
              "sources": [], "outputs": [], "collections": {},
              "deduplication": "preserve_all_source_observations",
              "limitations": ["No identity merge", "No external sharing authorization",
                              "Undated records kept separately; no invented time",
                              "Chronological order is for browsing, not inferred reply adjacency",
                              "No embeddings, summaries, claims, or old window memberships imported"]}
    manifest_path = out / "manifest.json"
    manifest_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    spool = sqlite3.connect(out / ".sort.sqlite3")
    spool.execute("CREATE TABLE observations (collection TEXT, segment TEXT, sort_key TEXT, source_order INTEGER, ordinal INTEGER, obs_id TEXT UNIQUE, payload TEXT)")
    # Used only to check native Discord duplicate/conflict IDs; no body is logged.
    seen_discord = {}
    duplicate_ids = conflict_ids = 0
    try:
        for source_order, table in enumerate(RAW_TABLES):
            metadata = tables[table]
            columns = [c["name"] for c in metadata["columns"]]
            if not set(RAW_FIELDS).issubset(columns):
                raise ValueError("Required raw table fields unavailable")
            digest, count = hashlib.sha256(), 0
            path = safe_bundle_path(bundle, metadata["rows_file"])
            with gzip.open(path, "rb") as stream:
                for ordinal, line in enumerate(stream, 1):
                    digest.update(line); values = json.loads(line)
                    if len(values) != len(columns):
                        raise ValueError("Raw row width mismatch")
                    rec, sort = discord_record(dict(zip(columns, values)), table,
                                                metadata["sha256_uncompressed"], ordinal,
                                                source_manifest["snapshot_tso"])
                    native = rec["native_message_id"]
                    body_hash = hashlib.sha256(canonical(rec["raw_fields"]).encode()).hexdigest()
                    if native in seen_discord:
                        duplicate_ids += 1; conflict_ids += seen_discord[native] != body_hash
                    seen_discord[native] = body_hash
                    add(spool, rec, sort, source_order, ordinal)
                    count += 1
            if digest.hexdigest() != metadata["sha256_uncompressed"] or count != metadata["source_count"]:
                raise ValueError("Raw source hash/count mismatch")
            report["sources"].append({"kind": "discord_raw", "table": table, "records": count,
                                       "sha256": digest.hexdigest()})
        report["discord_native_id_duplicates"] = duplicate_ids
        report["discord_native_id_conflicts"] = conflict_ids
        for source_order, cfg in enumerate(config["sources"], len(RAW_TABLES)):
            if not re.fullmatch(r"[a-z0-9-]+", cfg["collection_id"]):
                raise ValueError("Invalid collection identifier")
            path = (project / cfg["path"]).resolve()
            if not path.is_relative_to((project / "data/kakao_raw").resolve()):
                raise ValueError("CSV source outside explicit raw data root")
            digest = file_hash(path)
            if digest != cfg["sha256"]:
                raise ValueError("CSV hash mismatch")
            count = 0
            with path.open(encoding="utf-8-sig", newline="") as stream:
                reader = csv.DictReader(stream)
                if reader.fieldnames != ["Date", "User", "Message"]:
                    raise ValueError("Unexpected CSV header")
                for ordinal, row in enumerate(reader, 1):
                    rec, sort = kakao_record(row, cfg, digest, ordinal)
                    add(spool, rec, sort, source_order, ordinal)
                    count += 1
            if count != cfg["records"] or file_hash(path) != digest:
                raise ValueError("CSV count changed or source modified during read")
            report["sources"].append({"kind": "kakao_csv", "path": cfg["path"], "records": count,
                                       "sha256": digest, "collection_id": cfg["collection_id"],
                                       "source_room_id": cfg["source_room_id"]})
        spool.commit()
        spool.execute("CREATE INDEX export_order ON observations(collection,segment,sort_key,source_order,ordinal)")
        groups = list(spool.execute("SELECT collection, segment, count(*) FROM observations GROUP BY collection, segment ORDER BY collection, segment"))
        for collection, segment, expected in groups:
            dest = out / collection / (segment + ".jsonl.gz")
            dest.parent.mkdir(exist_ok=True, mode=0o700)
            checksum, count = hashlib.sha256(), 0
            with dest.open("wb") as raw, gzip.GzipFile(fileobj=raw, filename="", mode="wb", mtime=0, compresslevel=3) as stream:
                for payload, in spool.execute("SELECT payload FROM observations WHERE collection=? AND segment=? ORDER BY sort_key,source_order,ordinal", (collection, segment)):
                    line = (payload + "\n").encode(); stream.write(line); checksum.update(line); count += 1
            if count != expected:
                raise ValueError("Export count mismatch")
            # Re-read the real generated file; validate all rows and declared scopes.
            recheck, read_count = hashlib.sha256(), 0
            with gzip.open(dest, "rb") as stream:
                for line in stream:
                    recheck.update(line); rec = json.loads(line); read_count += 1
                    if rec["collection_id"] != collection or rec["access"]["external_share_granted"]:
                        raise ValueError("Collection/access corruption")
                    if rec["content"] != rec["raw_fields"].get("content", rec["raw_fields"].get("Message")):
                        raise ValueError("Raw body preservation failed")
                    if rec["platform"] == "discord" and set(rec["raw_fields"]) != set(RAW_FIELDS):
                        raise ValueError("Unexpected processed field in Discord corpus")
            if recheck.digest() != checksum.digest() or read_count != count:
                raise ValueError("Output readback mismatch")
            report["outputs"].append({"file": str(dest.relative_to(out)), "records": count,
                                      "sha256_uncompressed": checksum.hexdigest(), "sha256_gzip": file_hash(dest),
                                      "bytes": dest.stat().st_size, "readback_verified": True})
            report["collections"][collection] = report["collections"].get(collection, 0) + count
        report["total_records"] = sum(report["collections"].values())
        if report["total_records"] != sum(x["records"] for x in report["sources"]):
            raise ValueError("Source/output reconciliation failed")
        report["status"] = "verified"
        manifest_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    finally:
        spool.close()
    # Owned transient sort store only; source files and completed outputs stay intact.
    (out / ".sort.sqlite3").unlink()
    print(json.dumps({"status": report["status"], "collections": report["collections"],
                      "total_records": report["total_records"], "legacy_derivatives_used": False}))
    return report


def add(spool, record, sort, source_order, ordinal):
    segment = "dated" if sort is not None else "undated"
    spool.execute("INSERT INTO observations VALUES (?,?,?,?,?,?,?)",
                  (record["collection_id"], segment, sort or "", source_order, ordinal,
                   record["observation_id"], canonical(record)))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--project", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, required=True)
    options = parser.parse_args()
    run(options.snapshot, options.config, options.project.resolve(), options.output)
