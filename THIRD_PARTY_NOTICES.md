# Third-party notices

Masamong의 자체 코드와 문서는 루트 [MIT License](LICENSE)로 배포됩니다. 이 문서는 배포물에 포함되는 제3자 코드와 에셋의 출처 및 별도 조건을 요약합니다. 각 라이선스 전문과 copyright 고지는 [THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt)에 보존됩니다.

## Go release binaries

`masamong`과 `masamongd`를 10개 배포 대상(darwin/arm64·amd64, linux/amd64·arm64·armv7·riscv64, windows/amd64·arm64, freebsd/amd64·arm64)별로 `go list -deps`한 합집합에는 39개 외부 모듈이 포함됩니다. 대상에 따라 일부 모듈만 링크됩니다.

| 라이선스 | 구성 요소 |
|---|---|
| MIT | Charmbracelet UI 모듈, displaywidth/uax29, go-humanize, go-colorful, isatty/runewidth, cancelreader, strftime, uniseg, terminfo |
| BSD 계열 | Go x/crypto·net·sync·sys·text, goquery·cascadia(공지 게시판 HTML 읽기), DiscordGo, WebSocket, UUID, dbus, edwards25519, modernc sqlite/libc/mathutil/memory 등 |
| Apache-2.0 | `fyne.io/systray` |
| MIT 또는 Apache-2.0 | `go.yaml.in/yaml/v3` (파일별 조건은 포함된 LICENSE와 NOTICE 참조) |
| MPL-2.0 | `github.com/go-sql-driver/mysql v1.10.1` |
| Public Domain / MIT | `modernc.org/sqlite`가 포함하는 SQLite와 sqlite-vec |

MPL-2.0은 파일 단위 조건을 가진 라이선스입니다. Masamong은 `go-sql-driver/mysql`을 수정하지 않고 사용하며, 해당 버전의 소스와 라이선스는 [upstream tag](https://github.com/go-sql-driver/mysql/tree/v1.10.1)에서 받을 수 있습니다. Masamong 자체 파일의 MIT 조건이 이 제3자 모듈의 MPL-2.0 조건을 대체하지 않습니다.

`modernc.org/libc`의 생성 코드와 제3자 고지는 모듈의 `LICENSE-3RD-PARTY.md`를 포함해 그대로 배포합니다. SQLite 본체는 public domain이고 sqlite-vec은 MIT입니다.

## Embedded web console

| 구성 요소 | 사용 | 라이선스 |
|---|---|---|
| Svelte 5 | 관리 GUI 런타임 | MIT |
| Lucide / Feather-derived icons | 관리 GUI 아이콘 | ISC / MIT |
| Pretendard 1.3.9 | 포함 WOFF2 글꼴 | SIL Open Font License 1.1 |

Pretendard의 OFL 전문은 배포물 전체 고지뿐 아니라 `ui/public/fonts/LICENSE-PRETENDARD.txt`에도 둡니다. npm lockfile의 74개 package license 필드는 MIT 54, MPL-2.0 12, Apache-2.0 4, ISC 2, BSD-3-Clause 1, OFL-1.1 1입니다. MPL 항목은 `lightningcss`와 플랫폼 빌드 바이너리이며 정적 GUI를 만드는 개발 단계에서만 사용됩니다.

## Brand artwork

`assets/brand/`의 기본 마스코트와 배너 그림은 Grok Imagine으로 생성한 뒤 프로젝트에서 후처리했습니다. 이 그림의 출처 표기는 **Created with Grok**입니다. 2026-09-30 OpenAI 이미지 생성 작업에서 `mong-lab`과 `mong-guide`는 정면 캐릭터 시트를 기준으로 새로 생성했고, `mong-guide`는 새 `mong-lab`의 색과 스타일도 참고했습니다. `mong-chart`와 `mong-coin`은 기존 포즈를 보정했습니다. README 배너와 나머지 기존 그림의 Grok 출처 표기는 유지합니다. 생성 원본과 제작 기록은 로컬 전용 `assets/brand/source/`에 보관합니다.

## Optional and remote development components

- Python 보조 도구의 선택 의존성: NumPy(BSD), PyMySQL(MIT). 제품 실행 파일에 포함되지 않습니다.
- `experimental/rag-leaf-parent`의 브라우저 샌드박스는 CDN에서 Pretendard 1.3.9(OFL-1.1), Marked 12.0.2(MIT), DOMPurify 3.1.6(Apache-2.0 또는 MPL-2.0)을 불러옵니다. 제품 관리 GUI 배포물에는 CDN 코드가 포함되지 않습니다.
- 외부 API SDK나 응답 데이터의 약관과 사용료는 오픈 소스 라이선스와 별개이며, 사용자가 선택한 공급자의 조건을 따릅니다.

## External data

봇이 외부 API에서 받아 보여 주는 데이터는 Masamong 배포물에 포함되지 않으며 각 공급자의 조건을 따릅니다.

- 날씨(해외·국내 예비): [Open-Meteo](https://open-meteo.com/) — 데이터 CC BY 4.0, 무료 API는 비상업용. 날씨 카드에 `Open-Meteo.com · CC BY 4.0`으로 표시합니다.
- 날씨·특보·지진(국내): 기상청 — 카드에 출처를 표시합니다.
- 주가(Finnhub), 환율(ExchangeRate-API): 데이터 공유·재배포 제한이 있습니다. [추후 작업](docs/BACKLOG.md)의 "외부 서비스 약관"을 보세요.

## 감사 재실행

```bash
python3 scripts/license_audit.py
```

의존성을 변경한 뒤 사람이 새 라이선스를 검토했다면 다음 명령으로 전문 묶음을 갱신합니다.

```bash
python3 scripts/license_audit.py --write
```

감사 범위와 판단 근거는 [docs/license-audit.md](docs/license-audit.md)에 기록합니다.
