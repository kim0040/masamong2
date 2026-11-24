# 그림

구조 문서의 큰 그림 네 개는 [D2](https://d2lang.com)로 그린다. 원본 `.d2`와 렌더한 SVG 두 벌(밝은 판, 어두운 판)을 함께 커밋한다. 나머지 그림은 각 페이지 안의 Mermaid 블록이다.

| 원본 | 쓰는 페이지 |
|---|---|
| [overview.d2](overview.d2) | [한눈에 보기](../README.md) |
| [layers.d2](layers.d2) | [시스템 구성](../system.md) |
| [memory.d2](memory.d2) | [기억](../memory.md) |
| [data-model.d2](data-model.d2) | [데이터 모델](../data-model.md) |

## 고치는 법

1. `.d2` 원본을 고친다. 모양(색, 테두리, 글자색)은 원본에 쓰지 않고 클래스 이름만 붙인다.
2. `make diagrams`로 다시 렌더한다. `d2`가 설치돼 있어야 한다. 눈으로 볼 PNG가 필요하면 `scripts/diagrams.sh --png <폴더>`를 쓴다. PNG는 커밋하지 않는다.
3. `make docs`를 돌린다.

- 렌더한 SVG 끝에는 원본과 스타일 파일의 sha256이 주석으로 찍힌다. `make docs`가 이 값을 원본과 비교하므로, 원본을 고치고 렌더하지 않으면 검사가 실패한다. CI에는 `d2`가 필요 없다.
- 글꼴은 Pretendard(SIL Open Font License 1.1)다. D2의 기본 글꼴에는 한글이 없어서, 그림이 쓰는 글자만 SVG에 넣는다. 글꼴 파일은 처음 렌더할 때 저장소 밖 캐시 폴더로 내려받고 해시를 확인한다.
- SVG의 배경은 투명하다. GitHub의 밝은·어두운 화면색 위에 그대로 놓인다.
- 페이지에는 이렇게 넣는다. 대체 글에는 그림이 말하는 내용을 적는다.

```html
<picture><source media="(prefers-color-scheme: dark)" srcset="diagrams/x-dark.svg"><img alt="…" src="diagrams/x-light.svg"></picture>
```

## 색과 클래스

두 종류의 그림이 같은 뜻으로 같은 색을 쓴다. D2 쪽 정의는 [_style-light.d2](_style-light.d2)와 [_style-dark.d2](_style-dark.d2)에 있다.

| 클래스 | 뜻 | 밝은 판 채움 / 테두리 |
|---|---|---|
| `entry` | 입구: 사람이나 바깥에서 들어오는 길, 운영자가 시작하는 일 | `#dbe7ff` / `#2563eb` |
| `core` | 핵심 처리 | `#fde3cf` / `#d9661a` |
| `memory` | 기억 산출물 | `#dcf3e3` / `#15803d` |
| `external` | 바깥 서비스 | `#ebe2fd` / `#7c3aed` |
| `store` | 저장, 원문, 기록 | `#e7e5e4` / `#57534e` |

D2에는 묶음 테두리(`group`), 보이지 않는 줄(`row`), 사람(`actor`), 약한 선(`weak`)이 더 있다.

Mermaid 흐름도는 아래 두 조각을 그대로 붙인다. 테마는 정하지 않는다. GitHub이 화면 테마에 맞춰 고르게 두고, 채움을 정한 상자에는 글자색도 함께 정해서 어두운 화면에서도 읽히게 한다.

```text
%%{init: {"flowchart": {"curve": "basis", "nodeSpacing": 36, "rankSpacing": 44}}}%%

  classDef entry fill:#dbe7ff,stroke:#2563eb,color:#1c1917
  classDef core fill:#fde3cf,stroke:#d9661a,color:#1c1917
  classDef memory fill:#dcf3e3,stroke:#15803d,color:#1c1917
  classDef external fill:#ebe2fd,stroke:#7c3aed,color:#1c1917
  classDef store fill:#e7e5e4,stroke:#57534e,color:#1c1917
```

- 흐름도는 위에서 아래(`flowchart TB`)로 그린다. 옆으로 긴 그림은 GitHub 본문 폭에 맞춰 줄어들어 글자가 작아진다.
- 시퀀스 그림은 참여자 폭을 줄이는 init 블록만 쓴다([메시지 흐름](../message-flow.md)).
