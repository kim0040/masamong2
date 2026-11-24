[한국어](README.md) | **English**

# Diagrams

The four large diagrams of the architecture pages are drawn in [D2](https://d2lang.com). Each has a Korean source `x.d2` and an English source `x.en.d2`, and each source is committed together with its two rendered SVGs (light and dark). Every other diagram is a Mermaid block inside its page.

| Korean source | English source | Used on |
|---|---|---|
| [overview.d2](overview.d2) | [overview.en.d2](overview.en.d2) | [At a glance](../README.en.md) |
| [layers.d2](layers.d2) | [layers.en.d2](layers.en.d2) | [System layout](../system.en.md) |
| [memory.d2](memory.d2) | [memory.en.d2](memory.en.d2) | [Memory](../memory.en.md) |
| [data-model.d2](data-model.d2) | [data-model.en.d2](data-model.en.d2) | [Data model](../data-model.en.md) |

## How to change one

1. Edit the `.d2` source. Looks (fill, border, text colour) are not written in a source; it only names classes. Change the Korean and the English source together: they are one translation group, so `make docs` fails when only one of them moved.
2. Render again with `make diagrams`. This needs `d2` installed. For PNGs to look at, use `scripts/diagrams.sh --png <folder>`. PNGs are not committed.
3. Once both languages are updated, record the pair with `make docs-record`, then run `make docs`.

- The end of each rendered SVG carries the sha256 of its source and of its style file in comments. `make docs` compares them with the files, so a source that was edited and not rendered fails the check. CI does not need `d2`.
- The font is Pretendard (SIL Open Font License 1.1). D2's default font has no Korean letters, so only the letters a diagram uses are embedded in its SVG. The font files are downloaded on the first render into a cache folder outside the repository, and their hashes are checked.
- The SVG background is transparent, so a diagram sits directly on GitHub's light or dark page colour.
- A page embeds a diagram like this. The alt text says what the diagram tells. An English page uses `x.en-light.svg` and `x.en-dark.svg`.

```html
<picture><source media="(prefers-color-scheme: dark)" srcset="diagrams/x-dark.svg"><img alt="…" src="diagrams/x-light.svg"></picture>
```

## Colours and classes

Both kinds of diagram use the same colour for the same meaning. The D2 definitions are in [_style-light.d2](_style-light.d2) and [_style-dark.d2](_style-dark.d2).

| Class | Meaning | Light fill / border |
|---|---|---|
| `entry` | Entrances: ways in for people or the outside, and work the operator starts | `#dbe7ff` / `#2563eb` |
| `core` | Core processing | `#fde3cf` / `#d9661a` |
| `memory` | Memory outputs | `#dcf3e3` / `#15803d` |
| `external` | Outside services | `#ebe2fd` / `#7c3aed` |
| `store` | Storage, raw text, records | `#e7e5e4` / `#57534e` |

D2 also has a group border (`group`), an invisible row (`row`), a person (`actor`) and a weak line (`weak`).

A Mermaid flowchart pastes the two pieces below as they are. No theme is set: GitHub picks one to match the page, and a box that sets a fill also sets its text colour so it stays readable on a dark page.

```text
%%{init: {"flowchart": {"curve": "basis", "nodeSpacing": 36, "rankSpacing": 44}}}%%

  classDef entry fill:#dbe7ff,stroke:#2563eb,color:#1c1917
  classDef core fill:#fde3cf,stroke:#d9661a,color:#1c1917
  classDef memory fill:#dcf3e3,stroke:#15803d,color:#1c1917
  classDef external fill:#ebe2fd,stroke:#7c3aed,color:#1c1917
  classDef store fill:#e7e5e4,stroke:#57534e,color:#1c1917
```

- Flowcharts run top to bottom (`flowchart TB`). A wide diagram is shrunk to GitHub's text width and its letters get small.
- The sequence diagram uses only an init block that narrows the participants ([message flow](../message-flow.en.md)).
