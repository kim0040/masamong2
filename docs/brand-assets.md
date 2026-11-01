# 브랜드 에셋

제품에서 사용하는 최종 로고, 아이콘, 마스코트와 배포 이미지는 `assets/brand/`에 있습니다. 기본 마스코트와 배너 그림은 Grok Imagine으로 만들고 후처리했습니다(**Created with Grok**). 2026-09-30 OpenAI 이미지 생성 작업에서 `mong-lab`과 `mong-guide`는 정면 캐릭터 시트를 기준으로 새로 생성했고, `mong-guide`는 새 `mong-lab`의 색과 스타일도 참고했습니다. `mong-chart`와 `mong-coin`은 기존 포즈를 보정했습니다. `mong-search`, `mong-oops`, `mong-lost`, `mong-party`, `setup-model-key`, `setup-protect`은 이전 버전으로 복원했습니다. 원본과 제작 기록은 로컬 전용 `assets/brand/source/`에 두며 Git에 포함하지 않습니다. 공개 고지는 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)에 있습니다.

## 디렉터리

| 경로 | 용도 |
|---|---|
| `logo/` | 로고 마크와 한글·영문 lockup |
| `app/` | macOS·Linux·Windows 앱 아이콘과 트레이 아이콘 |
| `web/` | favicon, PWA와 공유 이미지 |
| `mascot/` | 상태·빈 화면·기억 흐름에 쓰는 몽이 이미지 |
| `tutorial/` | 초기 설정 단계 삽화 |
| `motion/` | 로딩·눈 깜박임 에셋 |
| `deploy/` | README와 설치 패키지 이미지 |

관리 콘솔용 축소본은 다음 명령으로 만듭니다.

```bash
cd internal/transport/web/ui
python3 scripts/assets.py
npm run build
```

`scripts/assets.py`는 최종 PNG/SVG를 `ui/public/brand/`에 복사하거나 WebP로 줄입니다. `npm run build` 결과는 `ui/dist/brand/`에 포함됩니다.

README 배너와 OG 이미지는 검토된 마스코트 그림을 유지하면서 언어별 문구를 그리는 생성 스크립트로 만듭니다.

```bash
python3 assets/brand/generate_localized_promos.py
```

`deploy/readme-banner-{ko,en,ja,zh-Hans,es}.png`와 `web/og-image-{ko,en,ja,zh-Hans,es}.png`를 갱신합니다. 기존 `deploy/readme-banner.png`는 한국어 배너의 호환 경로입니다. 제품 표시 이름은 UI `app.name`과 맞춰 한국어에서 `마사몽`, 나머지 언어에서 `Masamong`을 사용합니다. 문구는 같은 의미의 언어별 카피를 사용하고, 이미지는 기존 합성 PNG의 마스코트 영역을 잘라 유지합니다.

스크립트는 Pillow를 사용합니다. 한국어 표시 이름은 Apple SD Gothic Neo, 나머지 제목의 `Masamong`은 Avenir Next로 통일합니다. 본문 카피는 Avenir Next, Hiragino Sans W3(일본어), PingFang SC(간체 중국어), Apple SD Gothic Neo(한국어)를 우선합니다. 다른 시스템에서 다시 만들 때는 스크립트의 `TITLE_FONT_FILES`와 `BODY_FONT_FILES`에 설치된 라틴·지역별 CJK 글꼴 경로를 지정합니다.

Discord 배너, DMG 배경, 설치 사이드바와 설정 삽화에는 번역할 글자가 없습니다. 이 그림들은 하나의 공용 에셋으로 유지합니다. 설정 삽화의 말풍선도 비어 있어 실제 문구를 넣지 않습니다.

## 사용 규칙

- 제품에서는 `assets/brand/`의 최종 에셋만 참조합니다.
- 제품 UI의 마스코트·설정 삽화에는 문구를 넣지 않고, UI 텍스트를 언어팩으로 현지화합니다.
- README 배너와 OG 이미지처럼 소개용 합성 이미지에는 현지화 문구를 넣을 수 있습니다. 언어별 파일은 `generate_localized_promos.py`로 만들고, 표시 이름과 슬로건의 뜻을 각 언어 UI 및 README 카피와 일치시킵니다.
- 장식 이미지는 빈 `alt`, 정보 이미지에는 해당 화면 언어의 대체 문구를 사용합니다.
- 새 에셋은 밝은·어두운 배경, 작은 크기와 고해상도 화면에서 확인합니다.
- 로고와 앱 아이콘을 바꾸면 웹, 트레이, macOS 앱, Linux 설치와 Windows 패키지를 함께 확인합니다.

## 2026-09-30 시각 검토 범위

이번 검토는 정적 래스터 112개(일러스트 34개, 아이콘 변형 78개)를 대상으로 했습니다. 새 포즈도 같은 흰 포메라니안 몽이로 읽혀야 합니다. 네 발은 포즈에 맞게 자연스럽게 연결하고, 중복되거나 몸통에서 튀어나온 발과 치마처럼 보이는 하단 털 모양은 피합니다. 관절과 무게중심이 납득되고 바닥에 안정적으로 닿는지 작은 크기와 밝고 어두운 배경에서 확인합니다.

| 묶음 | 검토한 에셋 | 확인 기준 |
|---|---|---|
| 마스코트 14종 | `mong-box`, `mong-chart`, `mong-coin`, `mong-guide`, `mong-hello`, `mong-lab`, `mong-lock`, `mong-lost`, `mong-oops`, `mong-party`, `mong-search`, `mong-sleep`, `mong-think`, `mong-tools` | 같은 얼굴과 비율, 자연스러운 네 발과 관절, 치마처럼 보이는 하단 털 모양이 없는지 확인 |
| 설정 삽화 6종 | `setup-data-folder`, `setup-discord`, `setup-first-room`, `setup-model-key`, `setup-protect`, `setup-test-chat` | 같은 캐릭터 비율, 동작과 발을 가리지 않는 소품 확인 |
| 모션 | `mong-loading-1`–`mong-loading-4`, `mong-loading-sprite`, `mong-loading.svg`, `mong-blink.svg` | 로딩은 몸을 고정하고 꼬리만 움직임. 눈 깜빡임은 로고 형태 유지 |
| 배포 이미지 | `dmg-background`(1x/2x), `installer-sidebar`(1x/2x), 언어별 `readme-banner` | 크롭·여백과 글자 대비·정렬 확인 |
| 아이콘·공유 이미지 | `logo/`, `app/`, `web/`, `discord/` 아이콘 변형, OG 이미지와 Discord 배너 | 작은 크기, 밝고 어두운 버전, 마스크와 글자 가독성 확인 |

기본 정면 캐릭터 시트와 기존 포즈를 기준으로 삼습니다. 포즈와 소품을 맞추는 과정에서 캐릭터의 얼굴이나 몸 비율을 새로 해석하지 않으며, 생성 이미지는 최종 반영 전에 위 기준으로 시각 검토합니다.
