#!/usr/bin/env python3
"""Narrated picture book: a short illustrated story with a voice-over for every page.

    theme ──text (JSON)──▶ title + pages ──image generation──▶ one illustration per page
                                         └─speech synthesis──▶ one narration per page
    ──▶ book/index.html (open it in a browser)

Usage:
    python3 picture_book.py "a lighthouse keeper who befriends a seal" --pages 4

Uses three kinds of model and three billing units: tokens, images, characters. Each page
links the request ids of its picture and narration, so every charge can be verified.
"""

import argparse
import base64
import html
import json
import os

from openmodel_common import Bill, client, pick_model, receipt_of

STORY_SCHEMA = {
    "type": "object",
    "properties": {
        "title": {"type": "string"},
        "style": {"type": "string"},
        "pages": {"type": "array", "items": {
            "type": "object",
            "properties": {"text": {"type": "string"}, "picture": {"type": "string"}},
            "required": ["text", "picture"]}},
    },
    "required": ["title", "style", "pages"],
}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("theme")
    ap.add_argument("--pages", type=int, default=4)
    ap.add_argument("--voice", default="fable")
    ap.add_argument("--out", default="book")
    args = ap.parse_args()

    c = client()
    text, image, tts = pick_model(c, "text"), pick_model(c, "image"), pick_model(c, "tts")
    bill = Bill(c)
    os.makedirs(args.out, exist_ok=True)

    raw = c.chat.completions.with_raw_response.create(
        model=text, max_completion_tokens=1200,
        response_format={"type": "json_schema", "json_schema": {"name": "story", "schema": STORY_SCHEMA}},
        messages=[{"role": "developer", "content":
                   f"Write a children's picture book of exactly {args.pages} pages. Each page: two or three "
                   "short sentences of story text, and a one-sentence description of its illustration "
                   "(characters, setting, action — no text in the picture). `style` is one visual style "
                   "for every illustration, e.g. 'soft watercolor, warm light'."},
                  {"role": "user", "content": args.theme}])
    r = raw.parse()
    story = json.loads(r.choices[0].message.content)
    bill.add("write story", text, request_id=raw.headers.get("x-request-id", ""),
             tokens=(r.usage.prompt_tokens, r.usage.completion_tokens))
    pages = story["pages"][:args.pages]
    print(f"{story['title']}  ({len(pages)} pages, style: {story['style']})")

    for i, page in enumerate(pages, 1):
        raw = c.images.with_raw_response.generate(
            model=image, size="1024x1024", response_format="b64_json",
            prompt=f"{page['picture']}, {story['style']}, children's book illustration")
        open(f"{args.out}/page{i}.png", "wb").write(base64.b64decode(raw.parse().data[0].b64_json))
        page["image_rid"] = raw.headers.get("x-request-id", "")
        bill.add(f"page {i} picture", image, request_id=page["image_rid"],
                 quantity=(receipt_of(raw.headers) or {}).get("quantity", 1))

        raw = c.audio.speech.with_raw_response.create(model=tts, voice=args.voice, input=page["text"],
                                                      response_format="mp3")
        raw.parse().write_to_file(f"{args.out}/page{i}.mp3")
        page["audio_rid"] = raw.headers.get("x-request-id", "")
        bill.add(f"page {i} narration", tts, request_id=page["audio_rid"], quantity=len(page["text"]))
        print(f"  page {i}: {page['text']}")

    body = "".join(f"""<section><img src="page{i}.png" alt="{html.escape(p['picture'])}">
<p>{html.escape(p['text'])}</p><audio controls src="page{i}.mp3"></audio>
<small>picture <code>{p['image_rid']}</code> · narration <code>{p['audio_rid']}</code></small></section>"""
                   for i, p in enumerate(pages, 1))
    total = sum(r["usd"] for r in bill.rows)
    open(f"{args.out}/index.html", "w").write(f"""<!doctype html><meta charset="utf-8">
<title>{html.escape(story['title'])}</title>
<style>body{{font:18px Georgia,serif;max-width:720px;margin:2em auto;padding:0 1em;background:#fbf8f1}}
section{{margin:2.5em 0}}img{{width:100%;border-radius:8px}}audio{{width:100%}}small{{color:#888;font:12px monospace}}</style>
<h1>{html.escape(story['title'])}</h1>{body}
<p><small>{len(bill.rows)} requests · ${total:.6f} · every request id above has a worker-signed receipt</small></p>""")
    print(f"\nOpen {os.path.abspath(args.out)}/index.html")
    bill.print()


if __name__ == "__main__":
    main()
