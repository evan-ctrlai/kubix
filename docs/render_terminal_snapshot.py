"""Render a 140x44 ANSI snapshot emitted by the Go demo test, not a mock UI.

Documentation-only dependencies: Pillow and Rich. No network or model access.
"""
from argparse import ArgumentParser
from pathlib import Path
import os

from PIL import Image, ImageDraw, ImageFont
from rich.cells import cell_len
from rich.console import Console
from rich.text import Text


def system_font(bold=False):
    name = 'consolab.ttf' if bold else 'consola.ttf'
    candidates = [
        Path(os.environ.get('WINDIR', 'C:/Windows')) / 'Fonts' / name,
        Path('/usr/share/fonts/truetype/dejavu') / ('DejaVuSansMono-Bold.ttf' if bold else 'DejaVuSansMono.ttf'),
        Path('/System/Library/Fonts/Menlo.ttc'),
    ]
    return next((p for p in candidates if p.is_file()), None)


parser = ArgumentParser(description=__doc__)
parser.add_argument('source', type=Path)
parser.add_argument('--font', type=Path, default=system_font())
parser.add_argument('--bold-font', type=Path, default=system_font(True))
args = parser.parse_args()
if not args.font or not args.bold_font:
    parser.error('Provide --font and --bold-font paths to installed monospace fonts.')
text = Text.from_ansi(args.source.read_text(encoding='utf-8'))
console = Console(width=140, color_system='truecolor', force_terminal=True)
regular = ImageFont.truetype(str(args.font), 17)
bold = ImageFont.truetype(str(args.bold_font), 17)
cw, ch, padding = 11, 24, 18
img = Image.new('RGB', (140*cw+2*padding, 44*ch+2*padding), '#08111e')
draw = ImageDraw.Draw(img)
x = y = 0
for segment in text.render(console):
    style = segment.style
    fg = style.color.get_truecolor().hex if style and style.color else '#d8e3f0'
    bg = style.bgcolor.get_truecolor().hex if style and style.bgcolor else '#08111e'
    font = bold if style and style.bold else regular
    for char in segment.text:
        if char == '\n':
            x = 0
            y += 1
            continue
        width = max(0, cell_len(char))
        if y < 44 and x < 140:
            px, py = padding+x*cw, padding+y*ch
            draw.rectangle((px, py, px+max(1, width)*cw, py+ch), fill=bg)
            draw.text((px, py+2), char, font=font, fill=fg)
        x += width
output = args.source.with_suffix('.png')
img.save(output)
print(f'Rendered actual Go terminal snapshot: {output}')
