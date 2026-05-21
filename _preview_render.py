from PIL import Image, ImageDraw, ImageFont
import datetime, textwrap

PRINTER_WIDTH_PX = 384
MARGIN = 4

def load_font(size):
    for path in ["C:/Windows/Fonts/consola.ttf", "C:/Windows/Fonts/cour.ttf",
                 "C:/Windows/Fonts/lucon.ttf", "C:/Windows/Fonts/arial.ttf"]:
        try:
            return ImageFont.truetype(path, size)
        except Exception:
            pass
    return ImageFont.load_default()

def draw_line(draw, font, text, y, align, w, font_size):
    if not text:
        return
    try:
        tw = font.getbbox(text)[2] - font.getbbox(text)[0]
    except Exception:
        tw = len(text) * (font_size // 2)
    x = max(0, (w - tw) // 2) if align == "center" else MARGIN
    draw.text((x, y), text, fill="black", font=font)

def draw_lv(draw, font, label, value, y, w, font_size):
    draw.text((MARGIN, y), label, fill="black", font=font)
    try:
        vw = font.getbbox(value)[2] - font.getbbox(value)[0]
    except Exception:
        vw = len(value) * (font_size // 2)
    draw.text((w - MARGIN - vw, y), value, fill="black", font=font)

def render(name, email, message, title="NEW MESSAGE",
           show_ts=True, show_email=True, show_id=True, footer="", font_size=18):
    lw = 32
    w  = PRINTER_WIDTH_PX
    ts = datetime.datetime.now().strftime("%a  %b %d  %I:%M %p").replace(" 0", "  ")
    font      = load_font(font_size)
    title_sz  = min(font_size * 2, 48)
    title_fnt = load_font(title_sz)
    line_h    = font_size + 8
    title_lh  = title_sz + 8
    div       = "─" * lw

    pre = [
        ("center", f"MESSAGE FOR {name.upper()[:lw]}"),
        ("left", ""),
        ("left", div),
        ("left", ""),
    ]
    if show_ts:
        pre += [("lv", "TIMESTAMP:", ts), ("left", "")]
    if show_email:
        pre += [("lv", "EMAIL:", email), ("left", "")]
    if show_id:
        pre += [("lv", "TRANSACTION #:", "#1  (preview)")]
    pre += [("left", ""), ("left", "")]

    post = [("left", ""), ("left", "")]
    for ln in (textwrap.wrap(footer, lw) if footer else []):
        post.append(("center", ln))

    msg_lines = []
    for para in message.split("\n"):
        msg_lines += textwrap.wrap(para, lw) or [""]
    msg_h = len(msg_lines) * line_h + 8

    title_block = line_h + title_lh + 2 * line_h
    total_h = 20 + title_block + len(pre) * line_h + msg_h + len(post) * line_h + 20

    img  = Image.new("RGB", (w, total_h), "white")
    draw = ImageDraw.Draw(img)
    y    = 20

    y += line_h
    try:
        tw = title_fnt.getbbox(title)[2] - title_fnt.getbbox(title)[0]
    except Exception:
        tw = len(title) * (title_sz // 2)
    draw.text(((w - tw) // 2, y), title, fill="black", font=title_fnt)
    y += title_lh + 2 * line_h

    for entry in pre:
        if len(entry) == 3:
            draw_lv(draw, font, entry[1], entry[2], y, w, font_size)
        else:
            draw_line(draw, font, entry[1], y, entry[0], w, font_size)
        y += line_h

    for ln in msg_lines:
        draw.text((MARGIN, y), ln, fill="black", font=font)
        y += line_h

    for entry in post:
        draw_line(draw, font, entry[1], y, entry[0], w, font_size)
        y += line_h

    draw.line([(0, y + 4), (w, y + 4)], fill="#ccc", width=1)
    return img

sizes = [("Small (14px)", 14), ("Medium (18px)", 18), ("Large (22px)", 22)]
gap, cols = 20, []
for label, sz in sizes:
    im    = render("Ada Lovelace", "ada@example.com",
                   "Hello from your website! This is a test message.", font_size=sz)
    strip = Image.new("RGB", (im.width, 24), "#1a1a2e")
    ImageDraw.Draw(strip).text((6, 5), label, fill="white", font=ImageFont.load_default())
    col   = Image.new("RGB", (im.width, strip.height + im.height), "white")
    col.paste(strip, (0, 0)); col.paste(im, (0, strip.height))
    cols.append(col)

out_h = max(c.height for c in cols)
out   = Image.new("RGB", (PRINTER_WIDTH_PX * 3 + gap * 4, out_h + gap * 2), "#0d0d0d")
x = gap
for col in cols:
    out.paste(col, (x, gap)); x += PRINTER_WIDTH_PX + gap
out.save("_preview.png")
