# Copies brand assets (assets/brand) into the console at display sizes. Run from the ui folder:
#   python3 scripts/assets.py
# Mascot and tutorial art are high-resolution PNGs; the console shows them at <= 240px, so it ships
# 480px mascot and 640px tutorial WebP — roughly a tenth of the bytes.
import os, shutil, glob
from PIL import Image

SRC = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..', '..', '..', '..', 'assets', 'brand'))
OUT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', 'public', 'brand'))

def webp(src, dst, width):
    im = Image.open(src).convert('RGBA')
    h = round(im.height * width / im.width)
    im = im.resize((width, h), Image.LANCZOS)
    im.save(dst, 'WEBP', quality=82, method=6)

for p in glob.glob(os.path.join(SRC, 'mascot', '*.png')):
    webp(p, os.path.join(OUT, 'mascot', os.path.basename(p)[:-4] + '.webp'), 480)
for p in glob.glob(os.path.join(SRC, 'tutorial', '*.png')):
    webp(p, os.path.join(OUT, 'tutorial', os.path.basename(p)[:-4] + '.webp'), 640)
for name in ['logo-mark.svg', 'logo-mark-dark.svg', 'logo-mark-16.svg', 'logo-mark-mono.svg', 'logo-mark-64.png', 'logo-mark-dark-64.png', 'logo-lockup-h-ko.svg', 'logo-lockup-h-en.svg', 'logo-lockup-h-ko-dark.svg', 'logo-lockup-h-en-dark.svg']:
    shutil.copy(os.path.join(SRC, 'logo', name), os.path.join(OUT, name))
for name in ['favicon.svg', 'favicon.ico', 'favicon-32.png', 'apple-touch-icon.png', 'pwa-icon-192.png', 'pwa-icon-512.png', 'pwa-icon-maskable-512.png']:
    shutil.copy(os.path.join(SRC, 'web', name), os.path.join(OUT, name))
shutil.copy(os.path.join(SRC, 'motion', 'mong-blink.svg'), os.path.join(OUT, 'mong-blink.svg'))
# loading: 4 frames → one horizontal WebP strip (CSS steps animation)
frames = [Image.open(os.path.join(SRC, 'motion', f'mong-loading-{i}.png')).convert('RGBA').resize((160, 160), Image.LANCZOS) for i in range(1, 5)]
strip = Image.new('RGBA', (160 * 4, 160))
for i, f in enumerate(frames):
    strip.paste(f, (160 * i, 0))
strip.save(os.path.join(OUT, 'mong-loading.webp'), 'WEBP', quality=85, method=6)
print('ok')
