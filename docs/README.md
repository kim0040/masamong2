# 문서

제품 소개와 설치 안내는 아래 5개 언어 README에서 볼 수 있습니다. 코딩 에이전트가 따라 설치하는 안내는 저장소 루트의 [INSTALL.md](../INSTALL.md)입니다. 화면은 [언어별 README 스크린샷 모음](images/readme/README.md)에 모았습니다. 사용자용 서비스 문서(설치, 사용법, 설정 레퍼런스, 문제 해결)는 한국어·영어 [GitHub 위키](https://github.com/kim0040/masamong2/wiki)에 있으며 원본은 [wiki/](wiki/Home.md) 폴더입니다(`main` 병합 시 자동 동기화). 이 색인의 나머지 운영·개발 문서는 한국어입니다.

| 언어 | README | 화면 미리보기 |
|---|---|---|
| English | [README.md](../README.md) | [화면 보기](images/readme/en/setup.png) |
| 한국어 | [README.ko.md](../README.ko.md) | [화면 보기](images/readme/ko/setup.png) |
| 日本語 | [README.ja.md](../README.ja.md) | [画面を見る](images/readme/ja/setup.png) |
| 简体中文 | [README.zh-Hans.md](../README.zh-Hans.md) | [查看截图](images/readme/zh-Hans/setup.png) |
| Español | [README.es.md](../README.es.md) | [Ver capturas](images/readme/es/setup.png) |

## 운영

| 문서 | 내용 |
|---|---|
| [MAINTAINING.md](MAINTAINING.md) | **유지보수 가이드(먼저 읽기)**: 작업 규칙, 제품 원칙, 결정 이력, 업데이트 패치 내는 법, 검증 명령, 함정 |
| [memory-operations.md](memory-operations.md) | 기억 블록 가져오기, 준비, 연결, 비용과 장애 처리 |
| [STORAGE.md](STORAGE.md) | SQLite 저장소, TiDB를 끈 이유, 이전 봇 개인 설정 가져오기 |
| [tools-and-languages.md](tools-and-languages.md) | 도구, 언어팩과 새 언어 추가 |
| [TEST_SANDBOX.md](TEST_SANDBOX.md) | 제품 콘솔과 RAG 테스트 샌드박스의 구분 |
| [RAW_DATA_POLICY.md](RAW_DATA_POLICY.md) | 원문 보존, 채널·인물·권한 분리 원칙 |
| [BACKLOG.md](BACKLOG.md) | 남은 일: 사용자 결정 대기 항목, 배포·업데이트 경로, 운영자가 할 일, 품질 개선 후보 |
| [KNOWN_ISSUES.md](KNOWN_ISSUES.md) | 알려진 문제: 아직 남은 문제와 근거, 고친 것 |

## 개발

| 문서 | 내용 |
|---|---|
| [ARCHITECTURE.html](ARCHITECTURE.html) | 시스템 구조도: 전체 구조, 답 하나의 흐름, 기억(RAG) 흐름, 코드 위치 (브라우저로 열기) |
| [REPO_MAP.md](REPO_MAP.md) | 디렉터리와 주요 패키지 지도 |
| [plan-discord-experience.md](plan-discord-experience.md) | 역사 문서(구현됨): Discord 사용 경험·운영 개선(2026-10-15): 예약 작업, 운세·공지 카드, 콘솔 페이지 전환 버그, 기다리는 동안 안내·팁, 운영 데이터 점검 |
| [plan-memory-conversation.md](plan-memory-conversation.md) | 기억·대화·도구 설계 근거(2026-10, 구현 완료): 의도, 설계 원칙, 기능별 설계. 코드 주석이 절 번호를 가리킴 |
| [admin-gui-ia.md](admin-gui-ia.md) | 현재 관리 콘솔 페이지와 구현 상태 |
| [brand-assets.md](brand-assets.md) | 배포에 쓰는 로고·아이콘·마스코트 |
| [license-audit.md](license-audit.md) | 의존성 라이선스 검사, 외부 서비스 약관 요약 |
| [README 스크린샷 모음](images/readme/README.md) | 다섯 언어 README 화면(연습 모드에서 촬영) |
| [rag/](rag/README.md) | 현재 검색 구조, 소스 수명주기와 평가 방법, [임베딩 폭 측정](rag/embedding-width-2026-10.md) |
| [adr/ADR-16-gui-default-svelte-console.md](adr/ADR-16-gui-default-svelte-console.md) | Svelte 관리 콘솔 결정 |
| [adr/ADR-17-derived-room-memory.md](adr/ADR-17-derived-room-memory.md) | 방 기억(L1·L2)에 근거가 붙은 짧은 파생 문장 저장, 무효화 규칙 |
| [adr/ADR-18-idle-room-unloading.md](adr/ADR-18-idle-room-unloading.md) | **초안(사용자 결정 대기)** 안 쓰는 방 내려놓기: 색인 하나를 상주 방 집합으로, 실험 근거, 구현 전 계약 쟁점 |
| [defensive-patterns.md](defensive-patterns.md) | 실제 버그에서 뽑은 방어 규칙(교체 객체 은퇴, 마지막 경계 확인, 예측 가능한 경로 금지 등) |
| [postmortems/](postmortems/README.md) | 사고 회고: 왜 모든 검사가 놓쳤고 무엇을 추가했나 |

## 히스토리 및 아카이브

| 디렉터리 | 내용 |
|---|---|
| `archive/` (저장소에 없음, 로컬 보관) | 프로젝트 초기 기술 계획서, 01~20 설계서, 과거 작업 지시서(work-orders), 감사 보고서(reviews), 데이터 인벤토리 등 히스토리 보관소 |
