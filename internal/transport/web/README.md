# 제품 관리 콘솔

이 폴더는 daemon에 내장되는 **제품 관리자 GUI**입니다. 기술 결정은 [ADR-16](../../../docs/adr/ADR-16-gui-default-svelte-console.md), 화면과 Action 대응은 [admin-gui-ia](../../../docs/admin-gui-ia.md) §9를 따릅니다.

## 구조

| 파일·폴더 | 역할 |
|---|---|
| `server.go` | `127.0.0.1` 전용 listener, session cookie, 보안 header, 1회용 로그인, 정상 재시작 session handoff |
| `api.go` | `/api/action`, 단일 소유자 session/login, SSE, upload, secret API |
| `spa.go` | `ui/dist`를 `go:embed`로 서빙하고 hash asset cache 및 `.br`/`.gz` 응답 처리 |
| `lite.go` | JavaScript가 동작하지 않을 때의 `/lite` 상태·일시 중지·재시작 화면 |
| `token.go` | bootstrap token과 2분짜리 1회용 launcher code |
| `ui/src` | Svelte 5 + TypeScript 관리 콘솔 원본 |
| `ui/scripts/strings.py` | 한국어·영어 UI 문자열의 생성 원본 |
| `ui/dist` | commit하고 Go binary에 embed하는 production build |

테스트 샌드박스 `experimental/rag-leaf-parent`(8765)와 제품 관리 콘솔(기본 127.0.0.1:8787)은 서로 다른 프로그램입니다. 원격 접근은 공개 bind가 아니라 `masamong connect`의 SSH 포워딩만 사용합니다.

## 요청 경계

- 화면은 Application Action만 호출하며 DB나 벡터 파일을 직접 열지 않습니다.
- 쓰기 요청은 authenticated session, exact same-origin, `X-CSRF-Token`을 모두 검사합니다.
- session cookie는 `HttpOnly`, TLS에서는 `Secure`이며 기본 수명은 7일입니다.
- 정상 `restart`만 `<data-root>/sessions.json`을 `0600`으로 저장합니다. 새 daemon이 만료 session을 제외하고 읽은 뒤 파일을 즉시 삭제합니다. stop, signal, logout 뒤에는 세션을 넘기지 않습니다. 세션 목록 API는 제공하지 않습니다.
- 설치 소유자 한 명이 관리하며 초대·역할·비밀번호 로그인은 제공하지 않습니다. `masamong open`은 이 컴퓨터에서, `masamong connect`는 SSH를 통해 원격 daemon에 연결합니다.
- `/api/secret`은 macOS Keychain을 우선 사용하고 사용할 수 없으면 data root 아래 권한 제한 파일에 저장합니다. Action과 config에는 `keychain:` 또는 `file:` 참조만 전달합니다. 값은 표준입력으로 keychain 도구에 넘겨 프로세스 목록에 노출하지 않습니다.
- `/api/upload`의 기존 `attachment` 목적과 `channels.attachments.*` Action은 서버 호환을 위해 유지하지만, 새 콘솔의 기본 자료 흐름은 `/blocks`에서 CSV·Discord 원문을 가져와 준비한 뒤 채널에 연결하는 방식입니다.
- Connections 화면은 모델 단가·실제/추정 비용·키 저장 위치·키 존재 여부를 표시합니다. Open-Meteo는 키 없이 동작하고 한국 날씨는 KMA 우선·장애 시 Open-Meteo fallback입니다. 공급자 일일 호출 횟수는 DB의 사용량 기록으로 재시작·설정 변경 뒤에도 유지됩니다.
- CSP, `X-Frame-Options: DENY`, `nosniff`, referrer 정책을 server middleware에서 적용합니다.

session ID, CSRF, cookie, API key, Discord token을 로그나 fixture에 출력하지 마세요.

## 프런트엔드 개발

```bash
cd internal/transport/web/ui
npm ci
npm run check
npm run build
```

`npm run build`는 문자열 생성과 검사, Svelte production build, 사전 압축, first-load gzip budget 검사를 함께 실행합니다. 결과인 `ui/dist`도 source와 함께 commit합니다.

문구를 바꿀 때 생성된 locale JSON을 직접 편집하지 말고 `ui/scripts/strings.py`를 수정합니다. 새 화면은 `routes.ts`와 `routes.json`, navigation, Action 권한, loading/empty/error 상태를 함께 연결합니다.

브랜드 에셋은 다음 명령으로 `assets/brand`에서 `ui/public/brand`에 복사·변환합니다.

```bash
python3 ui/scripts/assets.py
```

Pretendard는 OFL-1.1 전문 `ui/public/fonts/LICENSE-PRETENDARD.txt`와 함께 배포합니다.

## 테스트

```bash
go test ./internal/transport/web -count=1
cd ui
npm run check
npm run build
```

서버 테스트는 단일 소유자 session/CSRF/origin, 비밀번호·계정·초대·세션 목록 제거, 1회용 링크, 재시작 handoff, upload, gzip/cache를 포함합니다. GUI 변경은 실제 브라우저에서 키보드, 좁은 viewport, 긴 한국어·영어 문구, 오류 복구, 주요 Action까지 확인합니다.

이전 Go template/htmx 콘솔은 제거됐습니다. ADR-15와 과거 review의 template 경로는 역사 기록이고 현재 구현 경로가 아닙니다.
