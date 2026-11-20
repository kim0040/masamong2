# 코드 지도

기능이 코드 어디에 있는지 검색 없이 찾기 위한 생성 색인입니다. 리뷰어와 에이전트가 Action, Discord 명령, 채팅 도구, 로케일이 어디서 읽히고 처리되는지 한 표에서 봅니다. 색인은 코드와 카탈로그에서 읽어 만들므로 손으로 고치지 않습니다.

| 파일 | 내용 | 원본 |
|---|---|---|
| [actions.md](actions.md) | 모든 Action: 상태, 효과, CLI 명령, 콘솔 경로, 처리 함수 | `internal/catalog`, `internal/application` |
| [commands.md](commands.md) | Discord 애플리케이션 명령: 한국어 이름, 호출하는 Action, 옵션 | `internal/connectors/discord`, `locales/` |
| [tools.md](tools.md) | 모델이 보는 채팅 도구와 그 도구를 설명하는 언어팩 키 | `internal/tools`, `internal/lang/packs` |
| [locales.md](locales.md) | 메시지 카탈로그 키 접두사와 로케일별 누락 | `locales/` |

## 다시 만들기

`go run ./tools/codemap` 또는 `make codemap`이 표 파일을 다시 씁니다. `go run ./tools/codemap -check`는 쓰지 않고 비교만 하며, 커밋된 파일이 생성 결과와 다르면 실패합니다. 같은 비교를 `tools/codemap`의 테스트가 하므로 지도가 오래된 채로 머물면 `go test ./...`가 실패합니다.

## 기능별 찾아가기

기능 단위로 묶어 코드 위치를 안내하는 색인은 `features.md`에 있습니다.
