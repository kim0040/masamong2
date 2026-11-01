"""Lossless JSON envelopes for a private, offline TiDB migration snapshot."""
import base64
from datetime import date, datetime, time, timedelta
from decimal import Decimal
import json
import math


def encode(value):
    if value is None or isinstance(value, (str, bool, int)):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("Nonfinite SQL float requires explicit handling")
        return value
    if isinstance(value, (bytes, bytearray)):
        return {"$type": "bytes", "value": base64.b64encode(value).decode("ascii")}
    if isinstance(value, Decimal):
        return {"$type": "decimal", "value": str(value)}
    if isinstance(value, (datetime, date, time)):
        return {"$type": type(value).__name__, "value": value.isoformat()}
    if isinstance(value, timedelta):
        return {"$type": "timedelta_us", "value": (value.days * 86400 + value.seconds) * 1000000 + value.microseconds}
    raise TypeError("Unsupported SQL value type: " + type(value).__name__)


def decode(value):
    if not isinstance(value, dict):
        return value
    kind, raw = value["$type"], value["value"]
    readers = {"bytes": base64.b64decode, "decimal": Decimal,
               "datetime": datetime.fromisoformat, "date": date.fromisoformat,
               "time": time.fromisoformat, "timedelta_us": lambda v: timedelta(microseconds=v)}
    return readers[kind](raw)


def row_bytes(row):
    return (json.dumps([encode(v) for v in row], ensure_ascii=False,
                       allow_nan=False, separators=(",", ":")) + "\n").encode("utf-8")
