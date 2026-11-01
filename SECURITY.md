# Security policy

**In English:** Please report vulnerabilities privately, not in public issues: by email to the address below, or on GitHub with **Security → Report a vulnerability**. Include the version, reproduction steps and logs with secrets removed. Never send real user data or keys. Security fixes go to the current `main` and the latest v1.x release first.

## 취약점 제보

보안 또는 개인정보 문제는 공개 issue에 상세 내용을 올리지 말고 Maintainer **김현민**에게 이메일로 제보해 주세요.

- Email: [mini0227kim@gmail.com](mailto:mini0227kim@gmail.com)
- 또는 GitHub 저장소의 **Security → Report a vulnerability**로 비공개 제보할 수 있습니다.
- 제목 예시: `[Masamong Security] 간단한 요약`

가능하면 영향을 받는 버전, 재현 조건, 예상 영향, 로그에서 비밀값을 제거한 증거를 포함해 주세요. 실제 사용자 데이터, API 키, Discord token, 비밀번호, session cookie는 보내지 마세요.

## 지원 범위

보안 수정은 현재 `main`과 최신 release(v1.x)를 우선합니다. 오래된 commit이나 개인 fork는 최신 버전에서 재현되는지 먼저 확인합니다.

## 운영 권장 사항

- 관리 콘솔은 기본 loopback bind를 유지하고 원격 관리는 SSH 터널을 사용합니다.
- 로그인 링크와 세션 파일을 다른 사람에게 전달하지 않습니다.
- credential은 `env:`, 시스템 자격 증명 저장소(macOS Keychain, Windows 자격 증명 관리자) 또는 소유자 전용 파일 참조로 전달합니다. 파일은 권한으로만 보호되고 암호화되지 않습니다.
- backup과 data root는 운영체제 권한(Windows에서는 서비스가 걸어 주는 소유자 전용 ACL)과 별도 디스크 암호화로 보호합니다. Discord ID와 운세 생년월일은 DB에 암호화되지 않은 채 들어 있습니다.
- 로그, 진단 결과, backup을 공유하기 전에 대화 본문과 식별자를 확인합니다.
