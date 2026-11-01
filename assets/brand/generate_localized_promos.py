#!/usr/bin/env python3
"""Regenerate localized README banners and social preview images.

The mascot artwork is cropped from the existing, reviewed compositions. This
script only lays out editable text and color fields; it never redraws artwork.
"""

from __future__ import annotations

from pathlib import Path
from shutil import copyfile

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[2]
BRAND = ROOT / "assets" / "brand"
DEPLOY = BRAND / "deploy"
WEB = BRAND / "web"

# app.name is localized to 마사몽 in Korean; the official Latin brand name is
# Masamong in English, Japanese, Simplified Chinese, and Spanish.
COPY = {
    "ko": {
        "name": "마사몽",
        "tagline": "이 대화에 필요한 기억만 꺼내요",
        "og_lines": ["이 대화에 필요한", "기억만 꺼내요"],
    },
    "en": {
        "name": "Masamong",
        "tagline": "Only the memory this conversation needs",
        "og_lines": ["Only the memory", "this conversation needs"],
    },
    "ja": {
        "name": "Masamong",
        "tagline": "この会話に必要な記憶だけを引き出す",
        "og_lines": ["この会話に必要な", "記憶だけを引き出す"],
    },
    "zh-Hans": {
        "name": "Masamong",
        "tagline": "只调取这段对话所需的记忆",
        "og_lines": ["只调取这段对话", "所需的记忆"],
    },
    "es": {
        "name": "Masamong",
        "tagline": "Solo los recuerdos que necesita esta conversación",
        "og_lines": ["Solo los recuerdos que", "necesita esta conversación"],
    },
}

# Keep the official Latin name identical across all non-Korean variants.
# Localized tagline fonts use the matching script/region where available.
TITLE_FONT_FILES = {
    "ko": [("/System/Library/Fonts/AppleSDGothicNeo.ttc", 6, 0)],
    "en": [("/System/Library/Fonts/Avenir Next.ttc", 0, 7)],
    "ja": [("/System/Library/Fonts/Avenir Next.ttc", 0, 7)],
    "zh-Hans": [("/System/Library/Fonts/Avenir Next.ttc", 0, 7)],
    "es": [("/System/Library/Fonts/Avenir Next.ttc", 0, 7)],
}

ASSETS_V2_FONTS = Path("/System/Library/AssetsV2/com_apple_MobileAsset_Font8")
PINGFANG_SC = next(ASSETS_V2_FONTS.glob("*/AssetData/PingFang.ttc"), None)
TITLE_FALLBACK_FONTS = [
    ("/Library/Fonts/Arial Bold.ttf", 0, 0),
    ("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", 0, 0),
    ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
]

BODY_FONT_FILES = {
    "ko": [
        ("/System/Library/Fonts/AppleSDGothicNeo.ttc", 6, 0),
        ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJKkr-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", 0, 0),
    ],
    "en": [
        ("/System/Library/Fonts/Avenir Next.ttc", 0, 7),
        ("/Library/Fonts/Arial Bold.ttf", 0, 0),
        ("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", 0, 0),
    ],
    "ja": [
        ("/System/Library/Fonts/ヒラギノ角ゴシック W3.ttc", 0, 0),
        ("/System/Library/Fonts/YuGothic-Medium.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansJP-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJKjp-Regular.otf", 0, 0),
        ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", 0, 0),
    ],
    "zh-Hans": [
        (str(PINGFANG_SC) if PINGFANG_SC else "", 11, 3),
        ("/System/Library/Fonts/STHeiti Medium.ttc", 0, 0),
        ("/System/Library/Fonts/Hiragino Sans GB.ttc", 2, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansSC-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", 0, 0),
        ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
    ],
    "es": [
        ("/System/Library/Fonts/Avenir Next.ttc", 0, 7),
        ("/Library/Fonts/Arial Bold.ttf", 0, 0),
        ("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", 0, 0),
    ],
}
FALLBACK_FONTS = {
    "ko": [("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0)],
    "en": [("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 0, 0)],
    "ja": [
        ("/usr/share/fonts/opentype/noto/NotoSansJP-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJKjp-Regular.otf", 0, 0),
        ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", 0, 0),
    ],
    "zh-Hans": [
        ("/usr/share/fonts/opentype/noto/NotoSansSC-Regular.otf", 0, 0),
        ("/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf", 0, 0),
        ("/System/Library/Fonts/Supplemental/Arial Unicode.ttf", 0, 0),
    ],
    "es": [("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 0, 0)],
}

ORANGE = (240, 138, 60)
CREAM = (253, 235, 221)
DARK = (59, 47, 42)
WHITE = (255, 255, 255)


def font(
    locale: str,
    size: int,
    *,
    bold: bool,
    purpose: str = "body",
) -> ImageFont.FreeTypeFont:
    choices = TITLE_FONT_FILES[locale] if purpose == "title" else BODY_FONT_FILES[locale]
    candidates = []
    for path, bold_index, regular_index in choices:
        if not path:
            continue
        candidates.append((path, bold_index if bold else regular_index))
    fallback_fonts = (
        TITLE_FALLBACK_FONTS
        if purpose == "title" and locale != "ko"
        else FALLBACK_FONTS[locale]
    )
    for path, bold_index, regular_index in fallback_fonts:
        candidates.append((path, bold_index if bold else regular_index))
    for path, index in candidates:
        if Path(path).is_file():
            try:
                return ImageFont.truetype(path, size=size, index=index)
            except OSError:
                continue
    raise RuntimeError(
        f"No usable font for {locale}. Install a CJK-capable font or update FONT_FILES."
    )


def fitted_font(
    image: Image.Image,
    text: str,
    locale: str,
    max_size: int,
    min_size: int,
    max_width: int,
    *,
    bold: bool,
    purpose: str = "body",
) -> ImageFont.FreeTypeFont:
    draw = ImageDraw.Draw(image)
    for size in range(max_size, min_size - 1, -1):
        candidate = font(locale, size, bold=bold, purpose=purpose)
        if draw.textbbox((0, 0), text, font=candidate)[2] <= max_width:
            return candidate
    raise RuntimeError(f"Text does not fit the available {locale} layout: {text}")


def make_banner(locale: str, artwork: Image.Image) -> Image.Image:
    image = Image.new("RGB", (1280, 320), ORANGE)
    image.paste(artwork, (0, 0))
    draw = ImageDraw.Draw(image)
    copy = COPY[locale]
    title = fitted_font(
        image, copy["name"], locale, 78, 56, 820, bold=True, purpose="title"
    )
    body = fitted_font(image, copy["tagline"], locale, 33, 25, 820, bold=False)
    draw.text((400, 80), copy["name"], font=title, fill=WHITE, anchor="lt")
    draw.text(
        (402, 187),
        copy["tagline"],
        font=body,
        fill=WHITE,
        anchor="lt",
        stroke_width=1 if locale in {"ko", "ja", "zh-Hans"} else 0,
        stroke_fill=WHITE,
    )
    return image


def make_og(locale: str, artwork: Image.Image) -> Image.Image:
    image = Image.new("RGB", (1200, 630), CREAM)
    image.paste(artwork, (0, 0))
    draw = ImageDraw.Draw(image)
    copy = COPY[locale]
    title = fitted_font(
        image, copy["name"], locale, 78, 56, 590, bold=True, purpose="title"
    )
    draw.text((560, 200), copy["name"], font=title, fill=DARK, anchor="lt")
    draw.rounded_rectangle((560, 318, 681, 327), radius=1, fill=ORANGE)

    lines = copy["og_lines"]
    size = 38
    while size >= 30:
        body = font(locale, size, bold=False, purpose="body")
        if all(draw.textbbox((0, 0), line, font=body)[2] <= 590 for line in lines):
            break
        size -= 1
    if size < 30:
        raise RuntimeError(f"OG tagline does not fit the {locale} text panel")
    for index, line in enumerate(lines):
        draw.text(
            (560, 351 + index * 43),
            line,
            font=body,
            fill=DARK,
            anchor="lt",
            stroke_width=1 if locale in {"ko", "ja", "zh-Hans"} else 0,
            stroke_fill=DARK,
        )
    return image


def main() -> None:
    banner_source = Image.open(DEPLOY / "readme-banner.png").convert("RGB")
    og_source = Image.open(WEB / "og-image-en.png").convert("RGB")
    if banner_source.size != (1280, 320) or og_source.size != (1200, 630):
        raise RuntimeError("Unexpected source composition dimensions")

    # These left-side source regions contain only the previously reviewed
    # mascot artwork and background, so reruns never sample the old text.
    banner_art = banner_source.crop((0, 0, 350, 320))
    og_art = og_source.crop((0, 0, 500, 630))

    for locale in COPY:
        make_banner(locale, banner_art).save(
            DEPLOY / f"readme-banner-{locale}.png", optimize=True
        )
        make_og(locale, og_art).save(WEB / f"og-image-{locale}.png", optimize=True)

    # Keep the long-standing Korean path for existing wiki or third-party links.
    copyfile(DEPLOY / "readme-banner-ko.png", DEPLOY / "readme-banner.png")


if __name__ == "__main__":
    main()
