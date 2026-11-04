"""Small dependency-free SVG line charts in the project's light theme."""

from __future__ import annotations

from html import escape

BG = "#FFFFFF"
GRID = "#E3ECF8"
TEXT = "#0B1B33"
MUTED = "#4A5B78"
COLORS = ["#2563EB", "#4C1D95", "#9F1239", "#0E7490", "#B45309", "#64748B"]


def _nice(v: float) -> float:
    if v <= 0:
        return 1.0
    import math

    e = 10 ** math.floor(math.log10(v))
    for m in (1, 2, 2.5, 5, 10):
        if v <= m * e:
            return m * e
    return 10 * e


def _fmt(v: float) -> str:
    if abs(v) >= 1e6:
        return f"{v / 1e6:g}M"
    if abs(v) >= 1e3:
        return f"{v / 1e3:g}k"
    return f"{v:g}"


def line_chart(series, title, subtitle="", xlabel="day", ylabel="", width=880, height=440, ymax=None, percent=False, xs=None, dashed=()):
    """series: list of (label, [y values]); x is the index unless xs is given."""
    left, right, top, bottom = 78, 250, 64, 52
    pw, ph = width - left - right, height - top - bottom
    n = max(len(v) for _, v in series)
    xs = xs or list(range(n))
    xmin, xmax = min(xs), max(xs)
    top_val = ymax if ymax is not None else _nice(max(max(v) for _, v in series) * 1.05)

    def X(x):
        return left + (x - xmin) / max(xmax - xmin, 1e-9) * pw

    def Y(y):
        return top + ph - (y / top_val) * ph

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" width="{width}" height="{height}" font-family="Inter, Segoe UI, Helvetica, Arial, sans-serif">',
        f'<rect width="{width}" height="{height}" fill="{BG}" rx="14"/>',
        f'<text x="{left}" y="28" font-size="17" font-weight="600" fill="{TEXT}">{escape(title)}</text>',
        f'<text x="{left}" y="48" font-size="12.5" fill="{MUTED}">{escape(subtitle)}</text>',
    ]
    for i in range(6):
        v = top_val * i / 5
        y = Y(v)
        label = f"{v * 100:g}%" if percent else _fmt(v)
        out.append(f'<line x1="{left}" y1="{y:.1f}" x2="{left + pw}" y2="{y:.1f}" stroke="{GRID}"/>')
        out.append(f'<text x="{left - 8}" y="{y + 4:.1f}" font-size="11" fill="{MUTED}" text-anchor="end">{label}</text>')
    step = _nice((xmax - xmin) / 6) if xmax > xmin else 1
    x = xmin - (xmin % step) if step else xmin
    while x <= xmax:
        if x >= xmin:
            out.append(f'<text x="{X(x):.1f}" y="{top + ph + 18}" font-size="11" fill="{MUTED}" text-anchor="middle">{_fmt(x)}</text>')
        x += step
    out.append(f'<text x="{left + pw / 2:.1f}" y="{height - 12}" font-size="11.5" fill="{MUTED}" text-anchor="middle">{escape(xlabel)}</text>')
    if ylabel:
        out.append(f'<text transform="translate(16 {top + ph / 2:.1f}) rotate(-90)" font-size="11.5" fill="{MUTED}" text-anchor="middle">{escape(ylabel)}</text>')
    for i, (label, ys) in enumerate(series):
        c = COLORS[i % len(COLORS)]
        stride = max(1, len(ys) // 400)
        pts = " ".join(f"{X(xs[j]):.1f},{Y(min(ys[j], top_val)):.1f}" for j in range(0, len(ys), stride))
        if (len(ys) - 1) % stride:
            pts += f" {X(xs[-1]):.1f},{Y(min(ys[-1], top_val)):.1f}"
        dash = ' stroke-dasharray="6 4"' if label in dashed else ""
        out.append(f'<polyline fill="none" stroke="{c}" stroke-width="2.2" stroke-linejoin="round"{dash} points="{pts}"/>')
        ly = top + 8 + i * 22
        out.append(f'<line x1="{left + pw + 18}" y1="{ly}" x2="{left + pw + 40}" y2="{ly}" stroke="{c}" stroke-width="3"{dash}/>')
        out.append(f'<text x="{left + pw + 46}" y="{ly + 4}" font-size="12" fill="{TEXT}">{escape(label)}</text>')
    out.append(f'<rect x="{left}" y="{top}" width="{pw}" height="{ph}" fill="none" stroke="{GRID}"/>')
    out.append("</svg>")
    return "\n".join(out) + "\n"
