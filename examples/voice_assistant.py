#!/usr/bin/env python3
"""Voice assistant: a spoken question in, a spoken answer out.

    speech  ──transcription──▶  text  ──chat──▶  answer  ──speech synthesis──▶  answer.mp3

Usage:
    python3 voice_assistant.py question.mp3          # any of WAV, FLAC, MP3, OGG, WebM, M4A
    python3 voice_assistant.py --say "How do Filecoin storage providers earn rewards?"

--say synthesizes the question first, so the example runs without a microphone.
One request of each kind is billed in its own unit: audio seconds, tokens, characters.
"""

import argparse
import os
import time

from openmodel_common import Bill, client, pick_model, receipt_of


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("audio", nargs="?", help="audio file with the spoken question")
    ap.add_argument("--say", help="text of the question; it is synthesized to question.mp3 first")
    ap.add_argument("--voice", default="nova", help="answer voice (an OpenAI voice name or an installed voice id)")
    ap.add_argument("--out", default="answer.mp3")
    ap.add_argument("--context", default="A question about Filecoin storage providers.",
                    help="a sentence in the style and vocabulary of the expected speech, passed as the "
                         "transcription prompt (helps the recognizer spell names such as Filecoin)")
    args = ap.parse_args()
    if not args.audio and not args.say:
        ap.error("give an audio file or --say TEXT")

    c = client()
    asr, text, tts = pick_model(c, "asr"), pick_model(c, "text"), pick_model(c, "tts")
    bill = Bill(c)

    if args.say:
        raw = c.audio.speech.with_raw_response.create(model=tts, voice="alloy", input=args.say,
                                                      response_format="mp3")
        raw.parse().write_to_file("question.mp3")
        bill.add("synthesize question", tts, request_id=raw.headers.get("x-request-id", ""),
                 quantity=len(args.say))
        args.audio = "question.mp3"

    t0 = time.time()
    with open(args.audio, "rb") as f:
        raw = c.audio.transcriptions.with_raw_response.create(model=asr, file=f, prompt=args.context)
    tr = raw.parse()
    seconds = (tr.usage.seconds if tr.usage else None) or (receipt_of(raw.headers) or {}).get("quantity", 0)
    bill.add("transcribe", asr, request_id=raw.headers.get("x-request-id", ""), quantity=seconds)
    print(f"You asked:  {tr.text}")

    raw = c.chat.completions.with_raw_response.create(
        model=text, max_completion_tokens=200,
        messages=[{"role": "developer", "content": "You are a voice assistant. Answer in two or three short "
                                                    "spoken sentences, no lists or markdown."},
                  {"role": "user", "content": tr.text}])
    chat = raw.parse()
    answer = chat.choices[0].message.content.strip()
    bill.add("answer", text, request_id=raw.headers.get("x-request-id", ""),
             tokens=(chat.usage.prompt_tokens, chat.usage.completion_tokens))
    print(f"Answer:     {answer}")

    raw = c.audio.speech.with_raw_response.create(model=tts, voice=args.voice, input=answer,
                                                  response_format="mp3")
    raw.parse().write_to_file(args.out)
    bill.add("speak answer", tts, request_id=raw.headers.get("x-request-id", ""), quantity=len(answer))
    print(f"Spoken answer: {os.path.abspath(args.out)}  ({time.time() - t0:.1f} s end to end)")
    bill.print()


if __name__ == "__main__":
    main()
