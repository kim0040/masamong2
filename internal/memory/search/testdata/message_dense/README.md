# message_dense fixture

A small, synthetic case for `retrieval.message_dense` that the Go search (`internal/memory/search`) and the
Python sandbox (`experimental/rag-leaf-parent`) both read, so they answer the same ranking questions. It
holds no real data and needs no numpy: plain JSON, six leaves of two messages each, plus a `projection`
section for the message→leaf mapping (overlap, ID order versus leaf order).

The Go replay is `message_dense_fixture_test.go` (`TestMessageDenseFixture`). The sandbox replay is
`experimental/rag-leaf-parent/test_message_vectors.py` (`FixtureParityTests`); it loads the same
`fixture.json`, runs `message_vectors.py` / `candidates.py` on each part below and compares. Do not edit the JSON by
hand when the behaviour changes: regenerate it from a reference implementation and review the diff.

## Conventions

- Every vector in the file is **raw**. Before use both sides L2-normalise each leaf row, each message row and
  the query with `v / (sqrt(sum(v*v)) + 1e-9)` (`search.Normalize`, the sandbox's `v /= norm + 1e-9`).
- Scores are cosines of normalised vectors. Go scores them in float32/float64 and the sandbox in numpy
  float32; compare scores with a tolerance of `1e-4` and fused (RRF) scores with `1e-7`. Rankings are exact.
  (The engine's loaded index stores message rows as int8, which costs up to about `2e-3` per cosine; the
  fixture's Go test uses float rows, so it is exact.)
- Ties are broken by row order (message rows in file order, leaves in file order). Row order is always the
  canonical one: leaf order, then message order inside the leaf (never observation ID or time). The fixture has two:
  `L2` and `L5` share a best message cosine, and `L3`/`L4` end up with the same fused score.
- The lexical side is empty by construction: no query word occurs in any leaf, so the leaf run is the dense
  one alone. A sandbox test that cannot arrange that can feed the expected `leaf_dense_ids` as the base
  ranking instead.
- RRF is `sum(weight / (60 + position))` with `position` 1-based, exactly `candidates.fuse` / `fuse` in Go.

## Fields

| field | meaning |
|---|---|
| `leaves` | leaf names in leaf order (`L0`..`L5`); row `i` of `leaf_vectors` belongs to `leaves[i]` |
| `leaf_vectors` | 4-dimensional leaf vectors |
| `message_vectors` | 12 message rows in order, each `{leaf, vec}` with a **2-dimensional** vector |
| `query` | the question vector, 4-dimensional; the message index is narrower, so it is cut to `message_dims` (2) components and **normalised again** before scoring (an uncut cosine would be too small) |
| `message_dims` | dimensionality of the message rows |
| `cases[]` | the run and fusion cases below |
| `guarantee_pool[]` | reserve-slot cases |
| `eligibility[]` | the short-message rule |
| `source_family` | `SourceFamily(name)` results |
| `projection` | the canonical message→leaf mapping, below |

### `cases[]`

Input: `allowed` (`null` = every leaf, a list = only those leaves, `[]` = nothing), `limit` (leaves in the
message run) and `rrf_weight` (weight of the message run in the final fusion; the base ranking has weight 1).

`expected`:

- `messages_scored`: message rows of the allowed leaves that were compared.
- `message_dense_ids` / `message_dense_scores`: the run. Score every allowed message row against the cut,
  re-normalised query; rank **leaves** by their best message (stable: among equal cosines the earlier row
  wins); keep the first `limit` leaves; a leaf's score is its best cosine.
- `leaf_dense_ids`: the base ranking, the leaves by cosine of their own vector with the (4-dimensional) query,
  restricted to `allowed`, ties by leaf order.
- `fused_ids` / `fused_scores`: `fuse([{base, 1.0}, {message_dense, rrf_weight}])`, stable on first
  appearance, so the base order wins a tie. With `allowed = []` everything is empty.

### `guarantee_pool[]`

`guarantee_pool(fusion, pool, per_source_min, reserve_max_frac)` on a fusion with the fused order `ordered`
and runs `runs`. `expected.guaranteed` is the reserve ids in the order they were claimed, `by_source` the same
grouped by run. The reserve is `int(pool * reserve_max_frac)` slots on top of the fused head. Runs claim in
sorted-name order, one position per round, and **`message_dense` claims last**, after all other runs, from
whatever reserve is left. That keeps leaf, lexical and every other source's slots exactly as they are without
the message run.

### `projection`

`leaves` (name + the observation ids it holds, in message order), `observation_vectors` (one raw vector per
observation that has one; `o9` has none, like a short message), `query`/`message_dims` as above, `expected_pairs`
and `cases[]` (same shape as the main cases, without the fusion part).

`ProjectMessages` (Go) / `project_message_rows` (Python) turn the leaves into the **pairs** (observation, leaf):
leaf order, then message order inside the leaf, keeping only observations that have a vector. A message in
several leaves (overlapping recipes) therefore yields one pair per leaf — `o1` and `o3` do here — while its
vector is computed once. The run scores every pair of an allowed leaf (`messages_scored` counts pairs), a leaf's
score is the best cosine of its pairs, and ties go to the earlier pair. The fixture makes this matter: `o3` sorts
after `o2` by ID but comes first in `P0`, and `P0`, `P1` and `P4` tie, so ranking by ID or projecting a message
onto only its last leaf gives another order.

### `eligibility[]`

`meaningful_runes(text)` and `eligible = meaningful_runes >= 6` (`search.MeaningfulRunes`,
`search.MessageEligible`, `search.MinMessageRunes`). A rune counts **only if it is a Unicode letter (L*) or number
(N*)**, and not a Hangul compatibility jamo (U+3131-U+318E: laughter such as the repeated k and h letters, the
keyboard noise of ㅇㅇ, ㅠㅠ). White space, control and format characters (variation selectors, the zero-width
joiner, ...), punctuation, symbols and emoji, combining marks, private-use and unassigned code points never count.
The rule applies to the message's own content, not to the speaker label.

A row has `text`, or `codepoints` (hex strings appended to `text`) for characters that cannot be shown or
survive an editor: zero-width joiners, variation selectors, keycap marks. `note` says what the row checks.
The rows cover Hangul syllables, compatibility jamo (excluded) and conjoining jamo (letters, counted),
CJK, Latin, full-width and half-width forms, digits, currency signs, emoji with joiners, selectors, skin
tones and flags, keycaps, combining marks, zero-width and no-break spaces. Go (`unicode`) and Python
(`unicodedata`) ship different Unicode versions, so a code point assigned as a letter or number in one and
still unassigned in the other can count in only one of them (rare; it only decides whether such a message gets
a vector). The fixture pins the boundary on purpose: U+1FAE9 (assigned after Python's Unicode 15) repeated is a
symbol in Go's tables and unassigned in Python's, and neither counts it.

## Related behaviour the sandbox must keep

- Off (`retrieval.message_dense.enabled` unset/false, or no message vectors loaded): no `message_dense` run,
  no trace key and no change to any existing output.
- Recovery (the second `generate`) scales the run length like the dense limit: `int(limit * limit_scale)`.
- `SourceFamily("message_dense") == "dense"`, so agreement signals do not count it as a second family.
- Trace, only when the run ran: `message_dense: {messages_scored, leaves, ms, dims}`.

## quantize_fixture.json

The int8 quantisation both sides apply to a message vector (scale = largest absolute component / 127 as float32,
components rounded half away from zero, clipped to +-127). Five rows of `dims` 8 whose values are exact in float16
and float32: half-way values (+-0.5, +-1.5, +-2.5, also after a scale of 2), values at the clip, a zero row and an
ordinary row. For each row: `values`, `float16_hex` (the same row as little-endian float16 words, as an earlier
version stored it), `q` (int8 components), `scale_hex` (the float32 scale, little endian) and `blob_hex`
(`search.EncodeQuantizedRow`: components, then the scale bytes). Go replays it in `TestQuantizeFixture`
(`message_quantize_fixture_test.go`, from the float32 values and from the float16 words); the sandbox in
`FixtureParityTests` (`test_message_vectors.py`: `quantize_rows`, and the loader's float16 branch must score the
rows as `q * scale`).

