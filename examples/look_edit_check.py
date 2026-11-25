#!/usr/bin/env python3
"""Look, edit, check: understand a picture, render a new version of it, and check the result.

    photo ──vision──▶ description ──text (JSON)──▶ image prompt ──image generation──▶ new.png
                                                                  new.png ──vision (JSON)──▶ verdict

Usage:
    python3 look_edit_check.py photo.jpg "as a watercolor painting at sunset"
    python3 look_edit_check.py --generate "a red bicycle leaning on a brick wall" "in winter, covered in snow"

--generate renders the starting picture too, so the example runs without a photo.
Writes original.png, new.png and report.html.
"""

import argparse
import base64
import html
import json
import mimetypes

from openmodel_common import Bill, client, pick_model, receipt_of


def data_url(img: bytes, mime: str = "image/png") -> str:
    return f"data:{mime};base64," + base64.b64encode(img).decode()


def ask_vision(c, model, bill, step, img, mime, question, schema=None):
    kw = {}
    if schema:
        kw["response_format"] = {"type": "json_schema", "json_schema": {"name": "answer", "schema": schema}}
    raw = c.chat.completions.with_raw_response.create(
        model=model, max_completion_tokens=300, **kw,
        messages=[{"role": "user", "content": [
            {"type": "text", "text": question},
            {"type": "image_url", "image_url": {"url": data_url(img, mime)}}]}])
    r = raw.parse()
    bill.add(step, model, request_id=raw.headers.get("x-request-id", ""),
             tokens=(r.usage.prompt_tokens, r.usage.completion_tokens))
    return r.choices[0].message.content


def generate(c, model, bill, step, prompt):
    raw = c.images.with_raw_response.generate(model=model, prompt=prompt, size="1024x1024",
                                              response_format="b64_json")
    img = base64.b64decode(raw.parse().data[0].b64_json)
    rc = receipt_of(raw.headers) or {}
    bill.add(step, model, request_id=raw.headers.get("x-request-id", ""), quantity=rc.get("quantity", 1))
    return img


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("image", nargs="?", help="starting picture (PNG / JPEG / WebP)")
    ap.add_argument("change", help="how the new version should differ")
    ap.add_argument("--generate", metavar="PROMPT", help="render the starting picture from this prompt")
    args = ap.parse_args()
    if not args.image and not args.generate:
        ap.error("give a starting image or --generate PROMPT")

    c = client()
    vision, text, image = pick_model(c, "vision"), pick_model(c, "text"), pick_model(c, "image")
    bill = Bill(c)

    if args.generate:
        original, mime = generate(c, image, bill, "render original", args.generate), "image/png"
    else:
        original = open(args.image, "rb").read()
        mime = mimetypes.guess_type(args.image)[0] or "image/png"
    open("original.png" if mime == "image/png" else "original" + (mimetypes.guess_extension(mime) or ""), "wb").write(original)

    # 1. Look: what is in the picture?
    description = ask_vision(c, vision, bill, "describe original", original, mime,
                             "Describe this picture in two sentences: subject, setting, colours, style.")
    print(f"Original:   {description}\n")

    # 2. Edit: turn description + requested change into one image prompt (structured output).
    raw = c.chat.completions.with_raw_response.create(
        model=text, max_completion_tokens=200,
        response_format={"type": "json_schema", "json_schema": {"name": "prompt", "schema": {
            "type": "object", "properties": {"prompt": {"type": "string"}}, "required": ["prompt"]}}},
        messages=[{"role": "developer", "content": "You write prompts for an image generator: one sentence, "
                                                    "concrete visual details, under 60 words."},
                  {"role": "user", "content": f"Picture: {description}\nChange: {args.change}\n"
                                              "Write the prompt for the changed picture."}])
    r = raw.parse()
    prompt = json.loads(r.choices[0].message.content)["prompt"]
    bill.add("write prompt", text, request_id=raw.headers.get("x-request-id", ""),
             tokens=(r.usage.prompt_tokens, r.usage.completion_tokens))
    print(f"Prompt:     {prompt}\n")

    new = generate(c, image, bill, "render new version", prompt)
    open("new.png", "wb").write(new)

    # 3. Check: does the new picture show what was asked?
    verdict = json.loads(ask_vision(
        c, vision, bill, "check new version", new, "image/png",
        f"The picture was meant to show: {prompt}\nDoes it? Judge subject, setting and style.",
        schema={"type": "object", "properties": {"matches": {"type": "boolean"}, "reason": {"type": "string"}},
                "required": ["matches", "reason"]}))
    print(f"Check:      {'matches' if verdict['matches'] else 'does NOT match'} — {verdict['reason']}")

    rows = "".join(f"<tr><td>{html.escape(r['step'])}</td><td>{html.escape(r['quantity'])}</td>"
                   f"<td>${r['usd']:.6f}</td><td><code>{html.escape(r['request_id'])}</code></td></tr>"
                   for r in bill.rows)
    open("report.html", "w").write(f"""<!doctype html><meta charset="utf-8"><title>Look, edit, check</title>
<style>body{{font:15px system-ui;max-width:980px;margin:2em auto}}img{{width:100%}}td{{padding:2px 10px}}</style>
<h1>Look, edit, check</h1><table><tr><td width="50%"><img src="data:{mime};base64,{base64.b64encode(original).decode()}">
<p>{html.escape(description)}</p></td><td><img src="data:image/png;base64,{base64.b64encode(new).decode()}">
<p><b>Prompt:</b> {html.escape(prompt)}</p></td></tr></table>
<p><b>Check:</b> {'✓ matches' if verdict['matches'] else '✗ does not match'} — {html.escape(verdict['reason'])}</p>
<h2>Requests</h2><table>{rows}</table>""")
    print("\nWrote original + new.png + report.html")
    bill.print()


if __name__ == "__main__":
    main()
