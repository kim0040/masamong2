"""Read-only full-corpus window membership audit; prints aggregates only.

No embeddings, API calls, model quality claims, or writes to the raw corpus.
"""
from __future__ import annotations

import argparse
from collections import Counter
import json

from corpus import collection_names, iter_observations
from windows import WindowConfig, build_windows


def verify(collection: str, config: WindowConfig) -> dict:
    expected = {}
    counts = Counter()
    rooms = set()

    def eligible():
        for obs in iter_observations(collection, kind='dated'):
            counts['dated_observations'] += 1
            if obs.semantic_eligible:
                key = (obs.platform, obs.collection_id, obs.source_room_id)
                if obs.observation_id in expected:
                    raise ValueError('duplicate source observation identity')
                expected[obs.observation_id] = key
                rooms.add(key)
                counts['eligible_observations'] += 1
                yield obs

    docs = build_windows(eligible(), config)
    seen = Counter()
    mixed = 0
    for doc in docs:
        key = (doc['platform'], doc['collection_id'], doc['source_room_id'])
        for ref in doc['observation_ids']:
            seen[ref] += 1
            mixed += expected.get(ref) != key
    result = dict(
        collection=collection, **counts, source_rooms=len(rooms), windows=len(docs),
        tail_windows=sum(doc['n'] < config.size for doc in docs),
        missing=len(set(expected)-set(seen)), unexpected=len(set(seen)-set(expected)),
        repeated_members=sum(n-1 for n in seen.values()), mixed_members=mixed,
        size=config.size, overlap=config.overlap,
        measurement_scope='raw_membership_only', model_calls=0,
    )
    if result['missing'] or result['unexpected'] or mixed:
        raise ValueError('window source membership audit failed')
    if config.overlap == 0 and result['repeated_members']:
        raise ValueError('unexpected duplicate window membership')
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--size', type=int, default=20)
    parser.add_argument('--overlap', type=int, default=0)
    args = parser.parse_args()
    config = WindowConfig(size=args.size, overlap=args.overlap).validate()
    for collection in collection_names():
        print(json.dumps(verify(collection, config)), flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
