import contextlib
import csv
import gzip
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from prepare_raw import RAW_FIELDS, RAW_TABLES, discord_record, kakao_record, run, timestamp


class RawCorpusTests(unittest.TestCase):
    def test_discord_drops_all_derived_fields_preserves_ids_and_bot(self):
        row = dict(zip(RAW_FIELDS, [2**64-1, 0, 12, 34, "같은 이름", "원문\n그대로", 1,
                                         "2026-01-02T10:30:00+09:00"]))
        row.update(embedding="forbidden", embedding_vec="forbidden", summary_text="forbidden")
        record, sort = discord_record(row, RAW_TABLES[0], "source", 1, "tso")
        self.assertEqual(set(record["raw_fields"]), set(RAW_FIELDS))
        self.assertNotIn("forbidden", json.dumps(record))
        self.assertEqual(record["native_message_id"], str(2**64-1))
        self.assertEqual(record["source_guild_id"], "0")
        self.assertEqual(record["record_kind"], "bot_record")
        self.assertEqual(sort, "2026-01-02T01:30:00+00:00")
        self.assertFalse(record["access"]["external_share_granted"])

    def test_marker_multiplicity_unknown_time_and_room_provenance(self):
        row = {"Date": "", "User": "", "Message": "unknown export marker"}
        old = {"collection_id": "friends-archive", "source_room_id": "old", "path": "old.csv"}
        a, sa = kakao_record(row, old, "hash1", 1)
        b, sb = kakao_record(row, old, "hash1", 2)
        new = dict(old, source_room_id="new", path="new.csv")
        c, _ = kakao_record(row, new, "hash2", 1)
        self.assertEqual(len({a["observation_id"], b["observation_id"], c["observation_id"]}), 3)
        self.assertIsNone(sa); self.assertIsNone(sb)
        self.assertEqual(a["time"]["timezone"], "unknown")
        self.assertNotEqual(a["source_room_id"], c["source_room_id"])

    def fixture(self, root):
        bundle = root / "snapshot"; bundle.mkdir()
        tables = []
        for index, table in enumerate(RAW_TABLES):
            fields = list(RAW_FIELDS) + ["embedding"]
            row = [100+index, 1, 2, 3, "tester", "raw body", index, "2026-01-02T10:00:00+00:00", "discard me"]
            data = (json.dumps(row) + "\n").encode()
            filename = table + ".jsonl.gz"
            with gzip.open(bundle / filename, "wb") as f:
                f.write(data)
            tables.append({"table": table, "columns": [{"name": name} for name in fields],
                           "sha256_uncompressed": hashlib.sha256(data).hexdigest(),
                           "source_count": 1, "rows_file": filename})
        # A discarded processed table need not even have a readable payload.
        tables.append({"table": "discord_memory_entries", "rows_file": "must-not-read.gz"})
        (bundle / "manifest.json").write_text(json.dumps({"status": "complete", "snapshot_tso": "fixture", "tables": tables}))
        raw = root / "data/kakao_raw"; raw.mkdir(parents=True)
        sources = []
        for name in ["old", "new"]:
            path = raw / (name + ".csv")
            with path.open("w", encoding="utf-8-sig", newline="") as f:
                writer = csv.writer(f); writer.writerow(["Date", "User", "Message"])
                writer.writerows([["2026-01-01 12:00:00", "same", 'comma,\nquote"'], ["", "", "marker"]])
            sources.append({"path": str(path.relative_to(root)), "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                            "records": 2, "collection_id": "friends-archive", "source_room_id": name})
        config = root / "sources.json"; config.write_text(json.dumps({"sources": sources}))
        return bundle, config

    def test_complete_pipeline_merge_replay_and_readback(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); bundle, config = self.fixture(root)
            with contextlib.redirect_stdout(io.StringIO()):
                a = run(bundle, config, root, root / "out1")
                b = run(bundle, config, root, root / "out2")
            self.assertEqual(a["collections"], {"discord": 2, "friends-archive": 4})
            self.assertEqual(a["outputs"], b["outputs"])
            self.assertFalse(a["legacy_derivatives_used"])
            self.assertEqual(a["total_records"], 6)
            with gzip.open(root / "out1/friends-archive/dated.jsonl.gz", "rt") as f:
                records = [json.loads(line) for line in f]
            self.assertEqual({r["source_room_id"] for r in records}, {"old", "new"})
            self.assertEqual(records[0]["content"], 'comma,\nquote"')
            with self.assertRaises(FileExistsError):
                run(bundle, config, root, root / "out1")

    def test_source_tamper_does_not_produce_verified_manifest(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); bundle, config = self.fixture(root)
            cfg = json.loads(config.read_text()); cfg["sources"][0]["sha256"] = "wrong"
            config.write_text(json.dumps(cfg))
            with self.assertRaisesRegex(ValueError, "CSV hash mismatch"):
                run(bundle, config, root, root / "out")
            self.assertEqual(json.loads((root / "out/manifest.json").read_text())["status"], "incomplete")

    def test_invalid_or_absent_dates_are_not_imputed(self):
        for value in ["", None, "not a date"]:
            result, sort = timestamp(value)
            self.assertIsNone(sort)
            self.assertEqual(result["raw"], value)
            self.assertEqual(result["timezone"], "unknown")


if __name__ == "__main__":
    unittest.main()
