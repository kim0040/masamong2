# 라이선스·비용 감사

기준일: 2026-09-24 (배포 대상 확장·외부 서비스 약관 2026-10-01)  
대상: Masamong `main`의 소스, 실제 release 바이너리 의존성, Svelte 배포물, 글꼴·아이콘·브랜드 에셋, 선택형 개발 도구

## 결론

- 프로젝트 자체 코드와 문서는 MIT로 배포할 수 있다.
- release에 포함되는 의존성에서 유료, 독점, 비상업, source-available, AGPL, 강한 GPL 라이선스를 발견하지 않았다.
- 실제 Go 바이너리에는 permissive 라이선스와 MPL-2.0 모듈 하나(`go-sql-driver/mysql`)가 있다. MPL은 파일 단위 조건을 지키고 해당 소스 입수 경로와 고지를 유지한다.
- Svelte 런타임, Lucide 아이콘, Pretendard 글꼴은 재배포 가능한 라이선스다. Pretendard OFL 전문을 글꼴과 같은 배포 경로에도 추가했다.
- npm의 MPL 항목은 `lightningcss` 빌드 도구와 플랫폼 바이너리다. 생성된 정적 CSS/JS가 MPL 소스 파일을 포함하는 구조는 아니다.
- 소프트웨어 의존성에는 구독이나 좌석별 비용이 없다. Discord, 모델, 날씨, 금융 등 외부 API를 사용하면 공급자별 요금과 약관이 적용될 수 있다.
- 외부 API의 데이터 약관은 코드 라이선스와 별개다. 2026-10-01 확인: Finnhub(개인용, 데이터 공유·재배포는 서면 승인 필요), ExchangeRate-API(데이터 재배포 금지), Open-Meteo 무료 API(비상업용만, CC BY 4.0 출처 표시). 정리와 남은 일은 [추후 작업](BACKLOG.md)의 "외부 서비스 약관".

이는 현재 checkout과 명시된 upstream 조건을 대상으로 한 엔지니어링 감사다. 새 의존성, 에셋, 공급자 약관이 추가되면 다시 검토해야 한다.

## 조사 방법

### 1. 프로젝트 소스와 포함 파일

- `git ls-files`와 확장자·헤더 검색으로 vendored 코드, 복사된 JavaScript, 글꼴, 이미지, generated 파일을 확인했다.
- `vendor/`는 없으며, 제품 프런트엔드는 `ui/dist`에 정적 빌드 결과를 추적한다.
- `.private/`, 실제 대화 원문, DB, 세션 파일은 라이선스 대상이나 배포물로 취급하지 않으며 Git 포함 여부를 별도로 검사한다.

### 2. Go

`go.sum` 전체는 테스트·도구·간접 해석용 모듈까지 포함할 수 있으므로 다음 두 층을 구분했다.

1. `go list -deps ./cmd/masamong ./cmd/masamongd`를 `scripts/release.sh`의 10개 대상(GOOS/GOARCH/CGO_ENABLED, armv7은 GOARM=7)마다 실행하고 합집합 39개 외부 모듈을 추출했다(2026-10-01: 이전에는 5개 대상만 보았으나 armv7·riscv64·Windows arm64·FreeBSD를 더해도 모듈은 같았다). 대상별 집합이 다르므로 실행 호스트와 관계없이 같은 결과가 나오도록 대상을 고정했다.
2. 각 module cache 루트의 `LICENSE*`, `LICENCE*`, `COPYING*`, `NOTICE*`를 읽고 배포용 전문 묶음에 포함했다.
3. `go list -m all`도 비교하여 release 경로에 링크되지 않은 테스트·개발 전용 모듈을 구분했다.

주요 별도 조건:

- `github.com/go-sql-driver/mysql v1.10.1`: [MPL-2.0](https://www.mozilla.org/en-US/MPL/2.0/). Mozilla의 [MPL FAQ](https://www.mozilla.org/en-US/MPL/2.0/FAQ/)가 설명하는 파일 단위 copyleft와 larger work 배포 조건에 따라 upstream 소스 경로와 라이선스를 제공한다.
- `modernc.org/sqlite v1.59.0`: [공식 mirror](https://github.com/modernc-org/sqlite)의 BSD-3-Clause, SQLite public domain, sqlite-vec MIT 고지를 포함한다.
- `modernc.org/libc v1.75.7`: [pkg.go.dev](https://pkg.go.dev/modernc.org/libc)의 BSD-3-Clause 표시와 module의 제3자 고지 전문을 확인했다. 생성 코드에 나타나는 GCC/glibc 원문 표시는 실제 package의 주 라이선스와 예외·제3자 고지를 함께 읽어 판단했다.

### 3. npm과 웹 배포물

`internal/transport/web/ui/package-lock.json`의 모든 74개 package entry에 license 필드가 있는지 검사했다.

| SPDX/표시 | package 수 | 배포 관계 |
|---|---:|---|
| MIT | 54 | 런타임 및 build |
| MPL-2.0 | 12 | lightningcss build tool과 플랫폼 패키지 |
| Apache-2.0 | 4 | build |
| ISC | 2 | Lucide 포함 |
| BSD-3-Clause | 1 | build |
| OFL-1.1 | 1 | Pretendard 글꼴 |

배포되는 핵심 런타임·에셋의 upstream 조건도 따로 확인했다.

- [Svelte](https://github.com/sveltejs/svelte): MIT
- [Lucide](https://github.com/lucide-icons/lucide/blob/main/LICENSE): ISC, 포함된 Feather-derived 부분은 MIT
- [Pretendard](https://github.com/orioncactus/pretendard/blob/main/packages/pretendard/dist/LICENSE.txt): SIL OFL-1.1

### 4. 선택형 Python과 CDN

- repo의 Python import를 따라 NumPy(BSD)와 [PyMySQL](https://github.com/PyMySQL/PyMySQL)(MIT)을 확인했다. 둘은 release 바이너리에 포함되지 않는다.
- 실험 샌드박스 HTML의 고정 CDN 버전을 확인했다: [Marked](https://github.com/markedjs/marked)(MIT), [DOMPurify](https://github.com/cure53/DOMPurify)(Apache-2.0 또는 MPL-2.0), Pretendard(OFL-1.1).

## 자동 검사

```bash
python3 scripts/license_audit.py
```

검사는 다음 상황에서 실패한다.

- 루트 MIT LICENSE가 없거나 바뀜
- 실제 release Go module에 license/notice 파일이 없음
- 아직 검토하지 않은 Go 주 라이선스 문구가 나타남
- npm package license 필드가 없거나 허용 목록 밖의 값이 나타남
- 현재 의존성에서 생성한 `THIRD_PARTY_LICENSES.txt`와 commit된 파일이 다름
- `ui/public/fonts`와 `ui/dist/fonts`의 Pretendard OFL 전문이 upstream 파일과 다름

GitHub Actions `ci` workflow의 `license` job이 모든 push·PR에서 이 검사를 실행한다. release archive의 고지 포함 여부는 `scripts/verify-release.sh`가 archive마다 확인한다.

새 버전의 의존성으로 바꿀 때는 차이를 사람이 먼저 읽은 뒤 실행한다.

```bash
python3 scripts/license_audit.py --write
git diff -- THIRD_PARTY_LICENSES.txt THIRD_PARTY_NOTICES.md docs/license-audit.md
```

## 배포 기준

- release archive: `LICENSE`, `THIRD_PARTY_NOTICES.md`, `THIRD_PARTY_LICENSES.txt` 포함
- macOS app: `Contents/Resources/Licenses/`에 같은 파일 포함
- 사용자 설치: `<prefix>/share/doc/masamong/` 또는 Windows 프로그램 폴더에 고지 보존
- 글꼴: `fonts/LICENSE-PRETENDARD.txt`를 정적 배포물에도 포함
- 프로젝트 MIT 표시는 제3자 파일의 원래 라이선스를 덮어쓰지 않음

의존성 추가 PR은 무료 여부, SPDX 식별자, 재배포 조건, source/notice 의무, 배포물 포함 여부를 함께 기록해야 한다.
