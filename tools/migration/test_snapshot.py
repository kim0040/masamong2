import contextlib
from datetime import date, datetime, time, timedelta, timezone
from decimal import Decimal
import gzip
import hashlib
import io
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

from snapshot_codec import decode, encode, row_bytes
from verify_snapshot import verify


class SnapshotTests(unittest.TestCase):
    def test_types_and_precision_roundtrip(self):
        values = [None, "", "한국어\ncomma,quote\"", 2**64-1, -1, 0, 1.25,
                  b"\x00\xff\x80", Decimal("12345678901234567890.00100"),
                  date(2026, 9, 14), time(23, 59, 59, 123456),
                  datetime(2026, 9, 14, 12, 30, tzinfo=timezone.utc),
                  timedelta(days=-2, seconds=1, microseconds=17),
                  '{"$type":"bytes","value":"SQL JSON stays text"}']
        restored = [decode(v) for v in json.loads(row_bytes(values))]
        self.assertEqual(restored, values)
        self.assertEqual(row_bytes(restored), row_bytes(values))

    def test_invalid_numbers_fail(self):
        for number in [float("nan"), float("inf"), float("-inf")]:
            with self.assertRaises(ValueError):
                encode(number)

    def make_bundle(self, root, status="complete", corrupt=False):
        root.mkdir()
        ddl = b"CREATE TABLE example (id BIGINT, value BLOB);\n"
        rows = row_bytes([2**63 + 1, b"\x00\xff"]) + row_bytes([2, None])
        (root / "example.schema.sql").write_bytes(ddl)
        with gzip.open(root / "example.rows.jsonl.gz", "wb") as stream:
            stream.write(rows if not corrupt else rows.replace(b"null", b'"x"'))
        manifest = {"status": status, "snapshot_tso": "synthetic", "tables": [{
            "table": "example", "columns": [{"name": "id"}, {"name": "value"}],
            "source_count": 2, "ddl_file": "example.schema.sql", "rows_file": "example.rows.jsonl.gz",
            "sha256_ddl": hashlib.sha256(ddl).hexdigest(),
            "sha256_uncompressed": hashlib.sha256(rows).hexdigest()}]}
        (root / "manifest.json").write_text(json.dumps(manifest))

    def test_staging_restore_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as folder:
            base = Path(folder); bundle = base / "bundle"; target = base / "target.sqlite3"
            self.make_bundle(bundle)
            with contextlib.redirect_stdout(io.StringIO()):
                verify(bundle, target)
            with sqlite3.connect(target) as connection:
                self.assertEqual(connection.execute("SELECT count(*) FROM source_rows").fetchone()[0], 2)
            with self.assertRaisesRegex(ValueError, "new staging"):
                verify(bundle, target)

    def test_partial_and_corrupt_bundles_rejected(self):
        for status, corrupt in [("incomplete", False), ("complete", True)]:
            with self.subTest(status=status, corrupt=corrupt), tempfile.TemporaryDirectory() as folder:
                base = Path(folder); bundle = base / "bundle"
                self.make_bundle(bundle, status=status, corrupt=corrupt)
                with self.assertRaises(ValueError):
                    verify(bundle, base / "target.sqlite3")
                self.assertFalse((bundle / "verification.json").exists())


if __name__ == "__main__":
    unittest.main()
