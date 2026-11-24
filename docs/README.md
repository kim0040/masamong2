# 문서: 여기서 시작

이 폴더는 마사몽의 운영·개발 문서다. 제품 소개와 설치는 저장소 루트의 README([English](../README.md) · [한국어](../README.ko.md) · [日本語](../README.ja.md) · [简体中文](../README.zh-Hans.md) · [Español](../README.es.md))에, 사용자용 안내(설치, 사용법, 설정 레퍼런스, 문제 해결)는 [GitHub 위키](https://github.com/kim0040/masamong2/wiki)에 있다. 위키의 원본은 [wiki/](wiki/Home.md) 폴더다.

## 무엇을 하려는가

| 하려는 일 | 먼저 읽을 것 |
|---|---|
| 구조를 3분 안에 파악하기 | [architecture/README.md](architecture/README.md) |
| 패치 작업 시작하기 | [MAINTAINING.md](MAINTAINING.md) → [기여 안내](../CONTRIBUTING.md) |
| "왜 이렇게 만들었나" 찾기 | [architecture/decisions.md](architecture/decisions.md) |
| 고칠 Action·명령·도구의 위치 찾기 | [code-map/](code-map/README.md) (생성된 색인) |
| 기억 블록을 만들고 운영하기 | [memory-operations.md](memory-operations.md) |
| 설치를 코딩 에이전트에게 맡기기 | 저장소 루트의 [INSTALL.md](../INSTALL.md) |

## 구조와 결정

| 문서 | 내용 |
|---|---|
| [architecture/](architecture/README.md) | 구조 문서: 한눈에 보기, [시스템 구성](architecture/system.md), [메시지 흐름](architecture/message-flow.md), [기억](architecture/memory.md), [데이터 모델](architecture/data-model.md), [콘솔과 운영](architecture/console-and-ops.md), [공급자와 도구](architecture/providers-and-tools.md), [결정 기록](architecture/decisions.md), [용어집](architecture/glossary.md) |
| [REPO_MAP.md](REPO_MAP.md) | 디렉터리 한 줄 지도 |
| [code-map/](code-map/README.md) | 생성된 코드 지도: Action, Discord 명령, 채팅 도구, 로케일 색인(소스와 어긋나면 테스트 실패) |
| [adr/](adr/ADR-16-gui-default-svelte-console.md) | 결정 기록 원본: [ADR-16](adr/ADR-16-gui-default-svelte-console.md) Svelte 관리 콘솔, [ADR-17](adr/ADR-17-derived-room-memory.md) 방 기억, [ADR-18](adr/ADR-18-idle-room-unloading.md) 안 쓰는 방 내려놓기(초안, 결정 대기) |
| [rag/](rag/README.md) | 검색 구조, 소스 수명주기, 평가 방법, [임베딩 폭 측정](rag/embedding-width-2026-10.md) |
| [RAW_DATA_POLICY.md](RAW_DATA_POLICY.md) | 원문 보존, 삭제와 철회, 보관 기간 |
| [STORAGE.md](STORAGE.md) | SQLite 저장소, TiDB를 끈 이유, 백업·복원·초기화, 이전 봇 개인 설정 가져오기 |

## 운영

| 문서 | 내용 |
|---|---|
| [MAINTAINING.md](MAINTAINING.md) | **유지보수 가이드(패치 작업의 첫 문서)**: 작업 규칙, 제품 원칙, 패치 내는 법, 검증 명령, 함정 |
| [memory-operations.md](memory-operations.md) | 기억 블록 가져오기·준비·연결, 대화 흐름, 방 기억, 사람 기억, 유료 백필, 비용과 장애 처리 |
| [tools-and-languages.md](tools-and-languages.md) | 도구 켜고 끄기와 한도, HTTP 도구, 언어팩과 새 언어 추가 |
| [TEST_SANDBOX.md](TEST_SANDBOX.md) | 제품 콘솔과 검색 테스트 샌드박스의 구분 |
| [admin-gui-ia.md](admin-gui-ia.md) | 관리 콘솔의 페이지와 구현 상태 |
| [BACKLOG.md](BACKLOG.md) · [KNOWN_ISSUES.md](KNOWN_ISSUES.md) | 남은 일과 알려진 문제(2026-10-13 기준. 그 뒤는 릴리스 노트로 확인) |
| [release-notes/](../release-notes) | 버전별 릴리스 노트 |

## 배운 것과 참고

| 문서 | 내용 |
|---|---|
| [postmortems/](postmortems/README.md) | 사고 회고: 왜 모든 검사가 놓쳤고 무엇을 추가했나 |
| [defensive-patterns.md](defensive-patterns.md) | 실제 버그에서 뽑은 방어 규칙 |
| [license-audit.md](license-audit.md) | 의존성 라이선스 검사, 외부 서비스 약관 요약 |
| [brand-assets.md](brand-assets.md) | 로고·아이콘·마스코트 |
| [README 스크린샷 모음](images/readme/README.md) | 다섯 언어 README 화면(연습 모드에서 촬영) |

## 역사 문서

구현이 끝난 계획이다. 코드 주석이 절 번호를 가리키므로 구조를 그대로 둔다. 지금 동작은 위의 문서를 따른다.

| 문서 | 상태 |
|---|---|
| [plan-memory-conversation.md](plan-memory-conversation.md) | 기억·대화·도구 설계 근거(2026-10, 구현 완료) |
| [plan-discord-experience.md](plan-discord-experience.md) | Discord 사용 경험·운영 개선(2026-10-15, 구현됨) |
| [plan-fortune.md](plan-fortune.md) | 매일 운세 개선(2026-11-02, v1.1.0에서 구현됨) |

프로젝트 초기의 설계서와 작업 지시서는 저장소에 없다(`docs/archive/`, 로컬 보관).

## 문서를 고칠 때

- 고친 뒤 `make docs`를 돌린다(링크, 번역 기록, 명령 이름, 버전).
- 한국어 위키나 README를 고쳤으면 다른 언어도 고친 뒤 `make docs-record`를 돌린다.
- 구조가 바뀌면 [architecture/](architecture/README.md)의 해당 페이지를, 결정이 바뀌면 [결정 기록](architecture/decisions.md)을 함께 고친다.
- 코드 위치는 줄 번호가 아니라 파일 경로와 심볼 이름으로 가리킨다.
