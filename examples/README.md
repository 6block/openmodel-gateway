# OpenModel multimodal examples

Four small programs that use OpenModel's text, vision, image-generation, speech
recognition and speech synthesis models through the official OpenAI Python SDK. Each one
runs as written, picks its models from `GET /v1/models`, and prints what every request
cost, priced from `GET /v1/catalog`.

| Example | What it does | Models | Billing units |
|---|---|---|---|
| [`voice_assistant.py`](voice_assistant.py) | spoken question → transcription → answer → spoken answer | asr, text, tts | audio seconds, tokens, characters |
| [`look_edit_check.py`](look_edit_check.py) | describe a picture → write a prompt for a changed version → render it → check the result | vision, text, image | tokens, images |
| [`picture_book.py`](picture_book.py) | write a short story → one illustration and one narration per page → `book/index.html` | text, image, tts | tokens, images, characters |
| [`verify_billing.py`](verify_billing.py) | check every receipt's signature and asset hash against the bytes received, then the on-chain proof | image, asr, tts | images, audio seconds, characters |

## Setup

```bash
pip install openai cryptography
export OPENMODEL_API_KEY=sk-om-YOURKEY
export OPENMODEL_BASE_URL=https://openmodel.filfox.info/v1   # the default; set it for another gateway
```

An API key comes from the gateway's registration (`POST /v1/register`, or the web chat's
"Sign & get API key"), and requests need a deposit in the settlement contract — see the
gateway README. To use a particular model instead of the first one of its kind, set
`OPENMODEL_TEXT_MODEL`, `OPENMODEL_VISION_MODEL`, `OPENMODEL_IMAGE_MODEL`,
`OPENMODEL_ASR_MODEL` or `OPENMODEL_TTS_MODEL` to an id from `GET /v1/models`.

`openmodel_common.py` holds the shared helpers (client, model choice, receipt decoding,
cost table); the examples import it, so run them from this folder.

## Voice assistant

```bash
python3 voice_assistant.py --say "How do Filecoin storage providers earn rewards?"
```

`--say` synthesizes the question to `question.mp3` first, so no microphone is needed;
pass your own recording instead (`python3 voice_assistant.py question.m4a` — WAV, FLAC,
MP3, OGG, WebM and M4A are accepted). The answer is written to `answer.mp3`.

```
You asked:  How do Filecoin storage providers earn rewards?
Answer:     Filecoin storage providers earn rewards by storing data on their storage devices and
            getting paid in Filecoin tokens when clients rent space to them. They earn based on how
            much data they store, how long it's stored, and how reliably it's kept—proving data
            integrity through regular verification.
Spoken answer: /home/you/examples/answer.mp3  (3.4 s end to end)

  synthesize question        47 character  $0.000150  req-2fe512f65d3d1013
  transcribe               4 audio_second  $0.000024  req-1bb69ce8b0a95ebd
  answer                        42+59 tok  $0.000016  req-c99ec819959dff75
  speak answer              296 character  $0.000947  req-a1536aadc67dbf98
  total                                    $0.001138
```

`--context` is passed to the recognizer as the transcription `prompt`: a sentence in the
style and vocabulary of the expected speech. It is what makes names like "Filecoin" come
out spelled correctly; without it a small speech model may hear a similar common word.

## Look, edit, check

```bash
python3 look_edit_check.py --generate "a red bicycle leaning on a brick wall" "in winter, covered in snow"
python3 look_edit_check.py photo.jpg "as a watercolor painting at sunset"
```

The vision model describes the picture, a text model turns the description and the
requested change into one image prompt (a JSON-schema response, so the program can read
it reliably), the image model renders it, and the vision model checks the result against
the prompt, again as JSON. Output: `original.png`, `new.png` and `report.html` with both
pictures, the prompt, the verdict and the request table.

```
Check:      matches — The image successfully captures all key elements from the description: the
            vibrant red bicycle parked against a weathered, textured brick wall in winter, covered
            in fresh snow ...

  render original                1 image  $0.001880  req-ce5a2c996171bbd1
  describe original           425+57 tok  $0.000106  req-94b4e3e60fbd86a9
  write prompt                109+64 tok  $0.000028  req-1f7eaf9094c36e60
  render new version             1 image  $0.001880  req-68ce7dbfdc08e8b4
  check new version          484+100 tok  $0.000140  req-51651c23aae85d86
  total                                   $0.004034
```

A picture is sent as prompt tokens (about one token per thousand pixels), which is why the
two vision steps cost more than the text step.

## Narrated picture book

```bash
python3 picture_book.py "a lighthouse keeper who befriends a seal" --pages 2
```

A text model writes the title, a visual style and the pages (a JSON-schema response); each
page gets an illustration and an MP3 narration. Open `book/index.html` in a browser. Every
page lists the request ids of its picture and narration.

```
The Keeper and the Seal  (2 pages, style: soft watercolor, warm light)
  page 1: One snowy morning, the lighthouse keeper saw a seal shivering on the rocks. ...

  write story               90+206 tok  $0.000047  req-7424b5b40dcd9cf3
  page 1 picture               1 image  $0.001880  req-3cb5adb940ed90b2
  page 1 narration       130 character  $0.000416  req-9b525a4316d14375
  ...
  total                                 $0.004723
```

## Verifiable billing

```bash
python3 verify_billing.py
```

Makes one speech synthesis, one transcription and one image generation, and for each
checks, without trusting the gateway:

- the receipt in the `X-Om-Receipt` header carries a valid ed25519 signature of the worker
  that served the request;
- its `asset_hash` is the sha256 of the bytes you received (the WAV, the audio you
  uploaded, the PNG), and its `quantity` is what you were billed (characters, seconds
  rounded up, images).

```
Receipts (checked locally):
  speech         signature OK       asset_hash OK       quantity 70 OK    req-ed84d7bda5393bfb
  transcription  signature OK       asset_hash OK       quantity 5 OK    req-19d786875bf34658
  image          signature OK       asset_hash OK       quantity 1 OK    req-c772cc44bacf2768
```

With `--wait` it then waits for the requests to be settled on-chain and runs the
five-step proof (signature, Merkle leaf, inclusion proof, committed root, batch on chain)
with `verify-receipt.py` from the
[openmodel-contracts](https://github.com/6block/openmodel-contracts) repository:

```bash
python3 verify_billing.py --wait --verifier ../openmodel-contracts/verify-receipt.py \
    --proof-base https://openmodel.filfox.info --rpc https://rpc.ankr.com/filecoin \
    --contract 0x29b994f63D91e447945a1cDeA2776167B7415c26
```

Settlement runs on a fixed interval, so `--wait` can take as long as one interval; it
re-checks every minute and prints each proof as it becomes available, ending in
`RESULT: VERIFIED ✔` for every request.

## Notes

- Requests that reach a worker while its GPUs are busy with a Filecoin proof come back as
  `503` with a `Retry-After` header; the examples' client retries them automatically.
- The prices shown are computed from the live catalog. The amount actually settled is
  the one the signed receipts commit, which is what `verify_billing.py` checks.
