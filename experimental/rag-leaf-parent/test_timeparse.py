"""Every retrieval-path reader of an ISO time reads the same strings the same way."""

from __future__ import annotations

import unittest
from datetime import datetime, timezone

import engine as E
from retrieval_post import _parse_iso_ts
from segment import _parse_msg_ts
from soft_events import _parse_timestamp
from timeparse import normalize_iso, parse_epoch, parse_iso

READABLE = [
    "2026-03-05T10:20:30",
    "2026-03-05T10:20:30Z",
    "2026-03-05T10:20:30+09:00",
    "2026-03-05T10:20:30.250000+00:00",
    "  2026-03-05T10:20:30Z  ",
    "2026-03-05",
]
UNREADABLE = ["", "   ", "not a time", "2026-13-45T00:00:00", "Z", None, 12, ["2026-03-05"]]


class ParseIsoTests(unittest.TestCase):
    def test_a_trailing_z_is_utc_and_a_missing_offset_stays_naive(self) -> None:
        self.assertEqual(parse_iso("2026-03-05T10:20:30Z"), datetime(2026, 3, 5, 10, 20, 30, tzinfo=timezone.utc))
        self.assertIsNone(parse_iso("2026-03-05T10:20:30").tzinfo)
        self.assertEqual(parse_iso(" 2026-03-05T10:20:30+09:00 ").utcoffset().total_seconds(), 9 * 3600)

    def test_unreadable_is_none(self) -> None:
        for bad in UNREADABLE:
            self.assertIsNone(parse_iso(bad), bad)
            self.assertIsNone(parse_epoch(bad), bad)

    def test_normalize_keeps_the_rest_of_the_text(self) -> None:
        self.assertEqual(normalize_iso(" 2026-03-05T10:20:30Z "), "2026-03-05T10:20:30+00:00")
        self.assertEqual(normalize_iso("2026-03-05T10:20:30+09:00"), "2026-03-05T10:20:30+09:00")

    def test_a_time_outside_the_platform_range_never_raises(self) -> None:
        for text in ("0001-01-01T00:00:00", "9999-12-31T23:59:59"):
            parse_epoch(text)  # a float or None, depending on the platform; the contract is that it does not raise


class SitesAgreeTests(unittest.TestCase):
    def test_every_reader_gives_the_same_epoch(self) -> None:
        for text in READABLE + UNREADABLE:
            want = parse_epoch(text)
            self.assertEqual(E._parse_ts(text), want, text)
            self.assertEqual(_parse_iso_ts(text), want, text)
            if isinstance(text, str):
                self.assertEqual(_parse_msg_ts({"parsed_time": text}), want, text)
                self.assertEqual(_parse_timestamp(text), want, text)

    def test_the_source_date_is_the_date_in_the_text_not_in_this_computers_zone(self) -> None:
        self.assertEqual(E._source_date("2026-03-05T00:30:00+09:00"), "2026-03-05")
        self.assertEqual(E._source_date("2026-03-05T23:30:00Z"), "2026-03-05")
        self.assertEqual(E._source_date("2026-03-05T00:30:00"), "2026-03-05")
        self.assertEqual(E._source_date("garbage"), "")
        self.assertEqual(E._source_date(None), "")


if __name__ == "__main__":
    unittest.main()
