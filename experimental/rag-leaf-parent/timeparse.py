"""One way to read an ISO-8601 time string in the sandbox.

Every retrieval-path reader used to repeat "strip, turn a trailing Z into +00:00, fromisoformat" with its own error
policy. The rules here: surrounding spaces are ignored; a trailing `Z` means UTC; a string without an offset stays
naive (it is never silently put in this computer's zone, only `parse_epoch` does that when it asks for an epoch, as
`datetime.timestamp()` always has); anything unreadable is None. Callers that must raise on bad input use
`normalize_iso` and call `fromisoformat` themselves.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any


def normalize_iso(text: str) -> str:
    """Strip the text and write a trailing `Z` as `+00:00`."""

    text = text.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    return text


def parse_iso(value: Any) -> datetime | None:
    """The time in `value`, naive if it names no offset; None for a non-string, a blank or an unreadable string."""

    if not isinstance(value, str):
        return None
    text = normalize_iso(value)
    if not text:
        return None
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def parse_epoch(value: Any) -> float | None:
    """Seconds since the epoch; a naive time is read in this computer's zone. None when unreadable or out of range."""

    parsed = parse_iso(value)
    if parsed is None:
        return None
    try:
        return parsed.timestamp()
    except (ValueError, OverflowError, OSError):
        return None
