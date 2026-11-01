# Masamong 2 위키

**한국어** | [English](https://github.com/kim0040/masamong2/wiki/English)

![Masamong](https://github.com/kim0040/masamong2/raw/main/assets/brand/deploy/readme-banner-ko.png)

제품 소개와 설치 안내: [English](https://github.com/kim0040/masamong2/blob/main/README.md) · [한국어](https://github.com/kim0040/masamong2/blob/main/README.ko.md) · [日本語](https://github.com/kim0040/masamong2/blob/main/README.ja.md) · [简体中文](https://github.com/kim0040/masamong2/blob/main/README.zh-Hans.md) · [Español](https://github.com/kim0040/masamong2/blob/main/README.es.md) · [언어별 스크린샷 모음](https://github.com/kim0040/masamong2/blob/main/docs/images/readme/README.md)

**Masamong 2**는 Discord 대화와 메신저 대화 기록(카카오톡 내보내기 CSV 등)을 하나의 인스턴스에서 기억하고, 자연스럽게 대화를 이어가는 개인용 AI 어시스턴트 봇입니다. 설치 시 로컬 전용 **웹 관리 콘솔**이 함께 제공되어 채널별 정책, 기억 블록, AI 모델, 비용 예산, 백업 및 로그를 한곳에서 직관적으로 관리할 수 있습니다.

현재 버전: `0.2.0-dev` · 라이선스: MIT · 저장소: [kim0040/masamong2](https://github.com/kim0040/masamong2)

## 핵심 기능

| 영역 | 설명 |
|---|---|
| 대화 & 페르소나 | 채널별 응답 모드(항상 / 멘션 시 / 명령어 전용 / 비활성) 선택, 맞춤 페르소나 및 언어 규칙 지정. 한국어·영어·일본어·중국어(간체)·스페인어 기본 지원 |
| 기억 & 검색 | 실시간 대화를 수집하고 카카오톡 CSV를 색인하여, 채널마다 **명시적으로 인가된 기억 블록만** 안전하게 검색하여 답변에 활용 |
| 도구 연동 | 날씨, 지진·기상특보(자동 공지 포함), 주가, 환율, 웹 검색, 장소·맛집(카카오맵), 링크 읽기, 투표, 이미지 생성, 채널 요약, 수다왕 순위를 자연어 대화 또는 슬래시(`/`), 느낌표(`!`) 명령어로 호출. 도구가 답을 못 주면 오류 카드 대신 대화로 이어 감. 간단한 설정만으로 커스텀 HTTP 도구 확장 가능 |
| 개인화 편의 기능 | 자연어 알림 예약("내일 3시에 알려줘"), 아침 운세 DM 브리핑, 대학·편입 공지사항 맞춤 구독. 기능별 독립 동의 절차를 거치며 콘솔의 **사람·권한** 메뉴에서 철회·삭제·차단을 투명하게 관리 |
| 고성능 동시 처리 | 여러 채널의 요청은 **병렬로 동시** 처리하며, 개별 채널 내부에서는 **도착 순서대로** 답변 순서를 엄격히 보장 |
| 비용 & 사용량 관리 | 월간 예산 한도 설정, 도구별 일일/사용자별 호출 제한, 토큰 단가표 기반의 정밀한 비용 장부 제공 |
| 안정적 운영 환경 | 실시간 스트리밍 로그, 시스템 진단(`doctor`), 안전 백업 및 원클릭 복원, V1 레거시 설정 마이그레이션, 자동 시작 등록, 샌드박스 연습 모드 지원 |

## 빠른 시작 가이드

1. [설치](https://github.com/kim0040/masamong2/wiki/설치) — 운영체제별 권장 설치 방법
2. [빠른 시작](https://github.com/kim0040/masamong2/wiki/빠른-시작) — 관리 콘솔 접속부터 첫 대화까지 10분 완성
3. [Discord 연결](https://github.com/kim0040/masamong2/wiki/Discord-연결) — Discord 봇 애플리케이션 생성, 필수 권한 및 서버 초대
4. [기억 블록](https://github.com/kim0040/masamong2/wiki/기억-블록) — 카카오톡 대화 CSV 색인 및 채널별 기억 블록 연결

## 운영 및 관리 가이드

- [관리 콘솔](https://github.com/kim0040/masamong2/wiki/관리-콘솔) — 주요 메뉴별 기능 안내 및 SSH 터널 원격 접속 가이드
- [도구와 한도](https://github.com/kim0040/masamong2/wiki/도구와-한도) — 기본 도구 활성화, 사용량 한도 제어, 커스텀 HTTP 도구 연동
- [설정 레퍼런스](https://github.com/kim0040/masamong2/wiki/설정-레퍼런스) — `config.yaml` 설정 파일 전체 옵션 상세 안내
- [동시 처리와 성능](https://github.com/kim0040/masamong2/wiki/동시-처리와-성능) — 동시 응답 한도, 대화 순서 보장 원리, 채널 규모별 튜닝 가이드
- [부하 테스트와 장기 운영](https://github.com/kim0040/masamong2/wiki/부하-테스트와-장기-운영) — 스트레스 테스트 검증 결과, 리소스 용량 산정, 대화 기록 보존 정책
- [저장소와 백업](https://github.com/kim0040/masamong2/wiki/저장소와-백업) — SQLite 데이터베이스 아키텍처, 백업/복원 절차, 스토리지 설계 배경
- [CLI 명령어](https://github.com/kim0040/masamong2/wiki/CLI-명령어) — 전체 CLI 명령어 레퍼런스 및 스크립트 자동화용 JSON 출력 규격
- [언어 설정](https://github.com/kim0040/masamong2/wiki/언어-설정) — 다국어 답변 및 기억 언어팩 구성, 신규 언어 추가 방법
- [보안과 개인정보](https://github.com/kim0040/masamong2/wiki/보안과-개인정보) — 키 보관 위치(Keychain·자격 증명 관리자·소유자 전용 파일), 루프백 바인딩 원칙, 대화 원문 처리 가이드라인
- [문제 해결](https://github.com/kim0040/masamong2/wiki/문제-해결) — 봇 응답 누락 시 진단 절차 및 자주 묻는 질문(FAQ)

## 개발자 가이드

- [개발 가이드](https://github.com/kim0040/masamong2/wiki/개발-가이드) — 프로젝트 빌드, 테스트 실행, CI 파이프라인, 아키텍처 구조 및 기여 가이드

## 참고

마사몽은 대학생이 개인적으로 만들고 돌보는 프로젝트입니다. 보증·지원 없이 있는 그대로 제공되며, 외부 API 비용과 대화 기록 관리는 설치한 사람의 책임입니다. 자세한 내용은 README의 [면책 조항](https://github.com/kim0040/masamong2/blob/main/README.ko.md#면책-조항)을 보세요.

> [!NOTE]
> 본 위키 문서는 저장소의 `docs/wiki/` 디렉터리와 자동으로 동기화됩니다. 내용 수정이나 보완이 필요한 경우 해당 디렉터리의 문서를 수정하여 Pull Request로 제출해 주시기 바랍니다.
