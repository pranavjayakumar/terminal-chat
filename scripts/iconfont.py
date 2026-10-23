#!/usr/bin/env python3
"""Build the tc Symbols icon font from its pixel source.

    python3 scripts/iconfont.py assets/iconfont/icons.txt internal/iconfont/tc-symbols.ttf

icons.txt lists each icon as "CODEPOINT name" followed by its rows of an
8x16 cell ('#' on, '.' off). The output is byte-for-byte reproducible
(fixed timestamps), so internal/iconfont's test can check that the
committed font matches its source. fontTools is a dev-only tool
(pip install fonttools); tc itself only embeds the result.
"""
import sys

from fontTools.fontBuilder import FontBuilder
from fontTools.pens.ttGlyphPen import TTGlyphPen

UPM, ADV, ASC, DESC = 1000, 600, 800, 200  # the cell of JetBrains Mono, Menlo, SF Mono
PW, PH = ADV / 8, (ASC + DESC) / 16
TOP = 3  # the first bitmap row sits at grid row 3 of 16
EPOCH = 0x7C259DC0  # 2026-01-01 in the font's 1904-based seconds: fixed, for reproducible builds


def parse(path):
    icons, cur = [], None
    for line in open(path):
        line = line.rstrip('\n')
        if not line:
            continue
        if line[0] in '.#':
            cur[2].append(line)
        else:
            cp, name = line.split()
            cur = [int(cp, 16), name, []]
            icons.append(cur)
    return icons


def glyph(rows):
    pen = TTGlyphPen(None)
    for r, row in enumerate(rows):
        c = 0
        while c < 8:
            if row[c] != '#':
                c += 1
                continue
            e = c
            while e < 8 and row[e] == '#':
                e += 1
            y1 = ASC - (TOP + r) * PH
            y0 = y1 - PH
            x0, x1 = c * PW, e * PW
            pen.moveTo((x0, y0))
            pen.lineTo((x0, y1))
            pen.lineTo((x1, y1))
            pen.lineTo((x1, y0))
            pen.closePath()
            c = e
    return pen.glyph()


def build(src, out):
    icons = parse(src)
    names = ['.notdef', 'space'] + ['i_' + n for _, n, _ in icons]
    fb = FontBuilder(UPM, isTTF=True)
    fb.setupGlyphOrder(names)
    fb.setupCharacterMap({0x20: 'space', **{cp: 'i_' + n for cp, n, _ in icons}})
    empty = TTGlyphPen(None).glyph()
    fb.setupGlyf({'.notdef': empty, 'space': empty, **{'i_' + n: glyph(rows) for _, n, rows in icons}})
    fb.setupHorizontalMetrics({g: (ADV, 0) for g in names})
    fb.setupHorizontalHeader(ascent=ASC, descent=-DESC)
    fb.setupNameTable({'familyName': 'tc Symbols', 'styleName': 'Regular'})
    fb.setupOS2(sTypoAscender=ASC, sTypoDescender=-DESC, usWinAscent=ASC, usWinDescent=DESC, xAvgCharWidth=ADV)
    fb.setupPost(isFixedPitch=1)
    fb.font['head'].created = fb.font['head'].modified = EPOCH
    fb.font.recalcTimestamp = False
    fb.save(out)
    return len(icons)


if __name__ == '__main__':
    n = build(sys.argv[1], sys.argv[2])
    print(n, 'icons', len(open(sys.argv[2], 'rb').read()), 'bytes')
