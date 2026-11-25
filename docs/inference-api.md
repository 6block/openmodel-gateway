# OpenModel Gateway — Inference API

## Overview

The OpenModel Gateway provides an **OpenAI-compatible API** that routes inference requests across multiple Filecoin SP Workers. When a Worker begins mining (WindowPoSt/WinningPoSt), it is automatically removed from the routing pool, and requests are transparently redirected to available Workers.

Four kinds of inference are served, each on its own endpoint and billed in its own unit:

| Modality (`modality`) | Endpoint | Billing unit (`price_unit`) | Quantity billed |
|---|---|---|---|
| `text` | `POST /v1/chat/completions` (and legacy `/v1/completions`) | `token` | prompt + completion tokens |
| `vision` (image understanding) | `POST /v1/chat/completions` with OpenAI content arrays carrying `image_url` parts | `token` | prompt + completion tokens; images count as visual prompt tokens |
| `image` (image generation) | `POST /v1/images/generations` | `image` | images delivered |
| `asr` (speech recognition) | `POST /v1/audio/transcriptions` | `audio_second` | input audio duration, rounded **up** to whole seconds (minimum 1) |
| `tts` (speech synthesis) | `POST /v1/audio/speech` | `character` | input characters, counted as Unicode code points |

Which models are servable and what they cost changes as providers join and
switch: read both live from `GET /v1/models` (with `modality`, `price_unit`
and `endpoint` per model) and `GET /v1/catalog` (prices and limits). The
examples below pick a model of the right modality from the live list instead
of naming one.

**Base URL:** `http://<gateway-host>:3000`

**Hosted mainnet deployment (alpha):** `https://openmodel.filfox.info` —
publicly-trusted certificate, also serves the web UI and the public billing
queries on the same origin. Get a key via self-registration (settlement-api.md,
"Registration & API keys") and fund the wallet before calling.

**Authentication:** All requests require a Bearer token in the `Authorization` header.

```
Authorization: Bearer <CLIENT_TOKEN>
```

Each response includes an `X-Request-ID` header for request tracing. Additionally (when the
feature is enabled): `X-Session-Key` (with `X-Session-Id`, the session-key fingerprint —
see "Session affinity"); `X-Om-Receipt` (the signed billing receipt on non-streaming and
on every image / audio response — see "Receipts" below and settlement-api.md §2.2).

The command-line examples for the image and audio endpoints use two shell
variables and need `curl` and `jq`:

```bash
BASE=https://openmodel.filfox.info      # or http://<gateway-host>:3000
KEY=sk-om-YOURKEY                       # your API key
```

---

## Supported Endpoints

### Chat Completions (Primary)

```
POST /v1/chat/completions
```

Fully compatible with the [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat).

**Example:**

```bash
curl http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-4B-Instruct-2507",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is Filecoin?"}
    ],
    "max_tokens": 200,
    "temperature": 0.7
  }'
```

**Response:**

```json
{
  "id": "cmpl-abc123",
  "object": "chat.completion",
  "model": "Qwen/Qwen3-4B-Instruct-2507",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Filecoin is a decentralized storage network..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 24,
    "completion_tokens": 65,
    "total_tokens": 89
  }
}
```

### Text Completions (Legacy)

```
POST /v1/completions
```

Supported for backward compatibility. Same parameters as OpenAI's legacy completions API.

### List Models

```
GET /v1/models
```

Returns the list of available models across all active Workers. Each entry
says what kind of model it is, how it is billed and which endpoint serves it:

```json
{
  "object": "list",
  "data": [
    {"id": "Qwen/Qwen3-VL-8B-Instruct", "object": "model", "owned_by": "openmodel",
     "modality": "vision", "price_unit": "token", "endpoint": "/v1/chat/completions"},
    {"id": "segmind/SSD-1B", "object": "model", "owned_by": "openmodel",
     "modality": "image", "price_unit": "image", "endpoint": "/v1/images/generations"}
  ]
}
```

| Field | Values |
|---|---|
| `modality` | `text`, `vision`, `image`, `asr`, `tts` |
| `price_unit` | `token`, `image`, `audio_second`, `character` |
| `endpoint` | the endpoint that accepts this model |

Prices and per-model limits are in `GET /v1/catalog` (see "Prices and limits:
GET /v1/catalog" below). Sending a model to the wrong endpoint returns 400
naming the right one.

To pick a model of one modality in a script:

```bash
MODEL=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="image") | .id' | head -1)
echo "$MODEL"
```

### Image Understanding (vision chat)

```
POST /v1/chat/completions
```

A `vision` model takes the standard OpenAI content array: a user message's
`content` is a list of parts, `{"type": "text", "text": …}` and
`{"type": "image_url", "image_url": {"url": …}}`. Everything else about the
request — parameters, streaming, receipts, billing — is the same as text chat.

- **Image source**: send the image as a base64 `data:` URI
  (`data:image/png;base64,…`). An `http(s)://` URL is fetched by the serving
  worker, so it only works if that worker can reach it; a data URI always works.
  Any other URL form (for example `file://` or a plain path) returns 400
  `image_url must be a data: URI or an http(s):// URL`.
- **Formats**: PNG, JPEG, WebP and the other common formats Pillow decodes.
- **Size**: each decoded image may be at most **10 MiB**, and the whole request
  body at most the gateway's request cap (**10 MiB** by default,
  `max_request_bytes`) — base64 adds a third, so keep a single image under about
  7 MiB on the default cap. Over the body cap returns **413**.
- **Images per request**: each model has a cap (one image unless the provider
  configured more). `GET /v1/catalog` shows it as `max_images_per_request` when
  the operator publishes it; exceeding it returns **400**
  `too many images: N provided, this model accepts at most M per request`.
- **Text-only models** refuse image parts with **400**
  `model '…' does not accept image input`.
- **Billing**: per token, at the model's token prices. Each image is converted
  into visual prompt tokens by the model and is included in
  `usage.prompt_tokens`; larger images produce more tokens.

```bash
MODEL=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="vision") | .id' | head -1)
base64 < photo.png | tr -d '\n' > photo.b64
jq -n --arg model "$MODEL" --rawfile img photo.b64 '{
  model: $model, max_tokens: 64,
  messages: [{role: "user", content: [
    {type: "text", text: "What color is the main object? Answer in one word."},
    {type: "image_url", image_url: {url: ("data:image/png;base64," + $img)}}
  ]}]}' > vision.json
curl -s "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d @vision.json | jq .
```

Response (a normal chat completion; the image is inside `prompt_tokens`):

```json
{
  "id": "cmpl-f4012b8d",
  "object": "chat.completion",
  "model": "/models/Qwen--Qwen3-VL-8B-Instruct",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "Red"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 83, "completion_tokens": 2, "total_tokens": 85,
            "prompt_tokens_details": {"cached_tokens": 0}}
}
```

With the OpenAI Python SDK:

```python
import base64
from openai import OpenAI

client = OpenAI(base_url="https://openmodel.filfox.info/v1", api_key="sk-om-YOURKEY")
model = next(m.id for m in client.models.list().data if getattr(m, "modality", None) == "vision")
img = base64.b64encode(open("photo.png", "rb").read()).decode()
r = client.chat.completions.create(
    model=model,
    max_tokens=64,
    messages=[{"role": "user", "content": [
        {"type": "text", "text": "Describe this picture in one sentence."},
        {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img}"}},
    ]}],
)
print(r.choices[0].message.content)
```

### Image Generation

```
POST /v1/images/generations
```

OpenAI images API shape. JSON body:

| Field | Required | Notes |
|---|:---:|---|
| `model` | Yes | An `image` model from `GET /v1/models` |
| `prompt` | Yes | Text description; empty returns 400 `prompt is required` |
| `n` | No | Number of images, default 1. At most the model's `max_images_per_generation` (shown in `GET /v1/catalog`); more returns 400 `too many images: N requested, at most M` |
| `size` | No | `"WxH"` or `"auto"`, default `"1024x1024"` (`auto` is the same). Each side a multiple of 8 between 256 and 1024 (e.g. `512x512`, `768x1024`). A larger size such as `1792x1024` or `1536x1024` is scaled down to fit, keeping its aspect ratio (`1792x1024` renders 1024×584). Anything else returns 400 |
| `steps` | No | Denoising steps, between 1 and the worker's cap (50 by default); more returns 400 `too many steps: N requested, at most M`. When omitted the worker uses its default for the model: 4 for distilled few-step models (turbo, lightning, schnell), 25 for full models such as SDXL base. More steps render slower |
| `guidance` | No | Guidance scale (float); the model's default when omitted |
| `seed` | No | Integer seed; fixes the random starting point of the render |
| `response_format` | No | `b64_json` (the default) returns the image bytes in the response; `url` returns a link instead (see below). Any other value returns 400 |
| `output_format` | No | `png` (default), `jpeg` or `webp` |
| `output_compression` | No | 0–100, for `jpeg` / `webp`: higher is smaller and lossier (default 10) |
| `quality`, `style`, `user`, `background`, `moderation` | No | Accepted and ignored |

Response: each image is base64 in `data[].b64_json` (PNG unless `output_format`
says otherwise), and `usage.images` is the number of images delivered — the
billed quantity:

```json
{
  "created": 1794887025,
  "data": [{"b64_json": "iVBORw0KGgoAAAANSUhEUgAA…"}],
  "usage": {"images": 1}
}
```

With `response_format: "url"` each item carries `url` instead of `b64_json`:
a link on the gateway, `<gateway>/v1/files/images/<id>.png`, that serves the
image without authentication for **one hour**. The id is random and
unguessable; anyone holding the link can fetch the image during that hour.
The billed bytes and the receipt are the same as for `b64_json`.

Billing: `images delivered × unit_price_usd`. Before the render the gateway
reserves `n × unit_price_usd` against your balance and settles to the number
of images actually returned. The request is not streamed; it returns when the
render finishes (typically tens of seconds).

```bash
MODEL=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="image") | .id' | head -1)
curl -s "$BASE/v1/images/generations" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"prompt\":\"a lighthouse on a cliff at dusk, watercolor\",\"n\":1,\"size\":\"1024x1024\"}" \
  > image.json
jq .usage image.json
jq -r '.data[0].b64_json' image.json | base64 --decode > image.png
```

With the OpenAI Python SDK:

```python
import base64
from openai import OpenAI

client = OpenAI(base_url="https://openmodel.filfox.info/v1", api_key="sk-om-YOURKEY")
model = next(m.id for m in client.models.list().data if getattr(m, "modality", None) == "image")
r = client.images.generate(model=model, prompt="a lighthouse on a cliff at dusk, watercolor",
                           n=1, size="1024x1024")
open("image.png", "wb").write(base64.b64decode(r.data[0].b64_json))

r = client.images.generate(model=model, prompt="a lighthouse on a cliff at dusk, watercolor",
                           size="1792x1024", response_format="url")
print(r.data[0].url)   # valid for one hour
```

### Speech Recognition (transcription) and translation

```
POST /v1/audio/transcriptions
POST /v1/audio/translations
```

OpenAI audio API shape, `multipart/form-data`. `transcriptions` returns the
speech as text in its own language; `translations` returns it translated into
English. Both take the same fields and bill the same way.

| Field | Required | Notes |
|---|:---:|---|
| `file` | Yes | The audio file: **WAV** (PCM or float), **FLAC**, **MP3**, **OGG** (Opus or Vorbis), **WebM** (Opus) or **M4A / MP4** (AAC). Other formats return 400 |
| `model` | Yes | An `asr` model from `GET /v1/models` |
| `language` | No | Language code of the speech (e.g. `en`); detected automatically when omitted. Transcription only |
| `prompt` | No | Text in the style and vocabulary of the speech; helps spell names and terms (e.g. `"A question about Filecoin storage providers."`) |
| `temperature` | No | Sampling temperature of the recognizer, 0–1 |
| `response_format` | No | `json` (default), `text` (plain transcript), `verbose_json` (with timed `segments`), `srt` or `vtt` (subtitles) |
| `timestamp_granularities[]` | No | `segment` and/or `word`; `word` adds a `words` array (with `verbose_json`) |
| `stream` | No | `true` returns the result as server-sent events: `transcript.text.delta` per segment, then `transcript.text.done` with the full text and `usage` |

Limits:

- Upload size: at most **25 MiB** by default (`max_audio_upload_bytes`); over
  it returns **413**.
- Duration: at most the model's `max_audio_sec` (shown in `GET /v1/catalog`);
  longer audio returns 400 `transcription too large: N audio_second, at most M`.
- The file must declare its length in its container, as files written by
  standard encoders and recorders do: a FLAC header with the sample count, an
  MP4 / M4A file with its `moov` index (fragmented MP4 is not accepted), a
  WebM recording with its audio blocks. A file whose audio plays noticeably
  longer than its container declares returns 400 `audio plays Xs but its
  container declares Ys; re-encode the file`.

Response (`json`):

```json
{
  "text": "Storage providers lend idle graphics cards to inference.",
  "language": "en",
  "duration": 4.05,
  "usage": {"type": "duration", "seconds": 5}
}
```

`duration` is the audio length in seconds; `usage.seconds` is the billed
quantity — the duration **rounded up** to whole seconds, minimum 1 (4.05 s
bills 5 seconds). The gateway measures the duration itself from the uploaded
file's headers before the request is served, so the price is known up front:
`ceil(duration) × unit_price_usd`. For compressed formats the duration is the
length the file declares, which includes the encoder's few milliseconds of
padding.

`verbose_json` adds `task`, `segments` (`id`, `start`, `end`, `text`) and, with
`timestamp_granularities[]=word`, `words` (`word`, `start`, `end`). `srt` and
`vtt` return subtitle text with one cue per segment:

```
1
00:00:00,000 --> 00:00:04,000
Storage providers lend idle graphics cards to inference.
```

```bash
MODEL=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="asr") | .id' | head -1)
curl -s "$BASE/v1/audio/transcriptions" \
  -H "Authorization: Bearer $KEY" \
  -F model="$MODEL" -F file=@speech.wav | jq .
curl -s "$BASE/v1/audio/transcriptions" \
  -H "Authorization: Bearer $KEY" \
  -F model="$MODEL" -F file=@speech.mp3 -F response_format=srt
```

(`speech.wav` and `speech.mp3` can be any recordings — for example the output
of the speech synthesis example below.)

With the OpenAI Python SDK:

```python
asr = next(m.id for m in client.models.list().data if getattr(m, "modality", None) == "asr")
with open("speech.mp3", "rb") as f:
    r = client.audio.transcriptions.create(model=asr, file=f, prompt="Filecoin storage providers")
print(r.text, r.usage.seconds)
```

### Speech Synthesis

```
POST /v1/audio/speech
```

OpenAI audio API shape, JSON body:

| Field | Required | Notes |
|---|:---:|---|
| `model` | Yes | A `tts` model from `GET /v1/models` |
| `input` | Yes | The text to speak; empty returns 400 `input text is required` |
| `voice` | No | A voice id of the model (e.g. `af_heart`, `bm_george`, `zf_xiaoxiao`), or an OpenAI voice name — `alloy`, `ash`, `ballad`, `coral`, `echo`, `fable`, `nova`, `onyx`, `sage`, `shimmer`, `verse`, `marin`, `cedar` — which maps to a similar voice. The provider's default voice when omitted. An unknown voice returns 400 `unknown voice '…'; available: …` |
| `response_format` | No | `wav` (the default when omitted), `mp3`, `opus` (in Ogg), `aac` (ADTS), `flac` or `pcm` (raw 16-bit little-endian mono, 24 kHz) |
| `speed` | No | 0.25–4.0, default 1.0 |
| `instructions` | No | Accepted and ignored (the voice is chosen with `voice`) |

Limit: at most the model's `max_chars` characters (shown in
`GET /v1/catalog`); longer input returns 400
`speech too large: N character, at most M`.

The response body is the audio itself, with the matching `Content-Type`
(`audio/wav`, `audio/mpeg`, `audio/ogg`, `audio/aac`, `audio/flac`,
`audio/pcm`). Billing: the number of **Unicode code points** in `input` ×
`unit_price_usd` — counted by the gateway from your request, so the price is
known before you send it, whatever the format. Every code point counts once: a
letter, a space, a punctuation mark, one CJK character; an emoji built from
several code points counts as several.

```bash
MODEL=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="tts") | .id' | head -1)
curl -s "$BASE/v1/audio/speech" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"input\":\"Storage providers lend idle graphics cards to inference.\"}" \
  -o speech.wav
file speech.wav
curl -s "$BASE/v1/audio/speech" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"input\":\"Storage providers lend idle graphics cards to inference.\",\"voice\":\"nova\",\"response_format\":\"mp3\"}" \
  -o speech.mp3
file speech.mp3
```

The input above is 56 characters, so each request bills 56 `character` units.

With the OpenAI Python SDK (the SDK sends no `response_format` unless you set
one, so the reply is WAV unless you ask for another format):

```python
tts = next(m.id for m in client.models.list().data if getattr(m, "modality", None) == "tts")
client.audio.speech.create(model=tts, voice="nova", input="Hello from OpenModel.",
                           response_format="mp3").write_to_file("hello.mp3")
```

### Prices and limits: GET /v1/catalog

`GET /v1/catalog` (Bearer key) lists every model with its prices and limits.
Every entry has `id`, `available`, `modality` and `price_unit`; the remaining
fields depend on the unit:

| Field | Applies to | Meaning |
|---|---|---|
| `input_price_usd_per_1m`, `output_price_usd_per_1m`, `cache_read_price_usd_per_1m` | `token` | USD per 1,000,000 tokens (empty strings for other units) |
| `context_window`, `max_output` | `token` | Token limits (0 for other units) |
| `max_images_per_request` | `vision` | Images allowed in one chat request (when published) |
| `unit_price_usd` | `image`, `audio_second`, `character` | USD per one unit: per image, per second of audio, per character |
| `max_images_per_generation` | `image` | Largest `n` one request may ask for |
| `max_audio_sec` | `asr` | Longest audio one transcription may carry, in billed seconds |
| `max_chars` | `tts` | Longest `input` one speech request may carry, in characters |

```bash
curl -s "$BASE/v1/catalog" -H "Authorization: Bearer $KEY" \
  | jq '.data[] | {id, modality, price_unit, unit_price_usd, input_price_usd_per_1m, output_price_usd_per_1m}'
```

A per-unit entry looks like:

```json
{"id": "/models/Systran--faster-whisper-small", "available": true,
 "modality": "asr", "price_unit": "audio_second", "unit_price_usd": "0.000006",
 "max_audio_sec": 300, "max_output": 0, "context_window": 0,
 "input_price_usd_per_1m": "", "output_price_usd_per_1m": "", "cache_read_price_usd_per_1m": ""}
```

(The figures in examples are illustrative; the live catalog is
authoritative.) The response's top-level `fil_price_usd` is the FIL/USD rate
billing converts with.

### Receipts

Every successful image-generation, transcription and speech response — like
every non-streaming chat response — carries the worker-signed receipt in the
`X-Om-Receipt` header (base64 JSON). It has one of two shapes, decided by the
model's billing unit:

- **Token shape** (`text`, `vision`): `prompt_tokens`, `completion_tokens`,
  `cached_tokens` are the billed counts.
- **Asset shape** (`image`, `audio_second`, `character`): `quantity` is the
  billed quantity in the model's unit and `asset_hash` is the sha256 of the
  asset — the delivered PNG bytes (several images are hashed concatenated in
  response order), the delivered WAV for speech, or the **uploaded** audio file
  for a transcription. The token counts are 0.

A decoded asset receipt (speech):

```json
{
  "v": 1, "request_id": "req-0ddca08ffa5fac5c", "model": "/models/hexgrad--Kokoro-82M",
  "request_sha256": "dd0585…", "response_sha256": "28a9aa…",
  "prompt_tokens": 0, "completion_tokens": 0, "cached_tokens": 0,
  "ts": 1794887026, "pubkey": "ce436c…", "sig": "731ccd…",
  "quantity": 56, "asset_hash": "28a9aa16ae3de177cf22400de2dd7de80d77b834364faea43a04858f72eaa81b"
}
```

The gateway re-measures the quantity and the asset hash itself before billing,
and you can check the hash against the file you received:

```bash
curl -s -D headers.txt -o speech.wav "$BASE/v1/audio/speech" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"input\":\"Storage providers lend idle graphics cards to inference.\"}"
grep -i '^x-om-receipt:' headers.txt | cut -d' ' -f2 | tr -d '\r' | base64 --decode | jq .
shasum -a 256 speech.wav      # equals asset_hash
```

(`$MODEL` is the `tts` model picked in "Speech Synthesis" above.) Proving that
a receipt entered an on-chain settlement is described in settlement-api.md
§2.2.

---

## Supported Parameters

| Parameter | Supported | Notes |
|-----------|:---------:|-------|
| `model` | Yes | A specific model id from `GET /v1/models` (e.g. `"Qwen/Qwen3-4B-Instruct-2507"`). The `"default"` alias is **not accepted** — it returns 400 |
| `messages` | Yes | Standard chat message array |
| `max_tokens` / `max_completion_tokens` | Yes | Either spelling; `max_tokens` wins when both are sent |
| `temperature` | Yes | 0.0 - 2.0 |
| `top_p` | Yes | 0.0 - 1.0 |
| `stop` | Yes | String or array of stop sequences |
| `stream` | Yes | SSE streaming supported; `stream_options: {"include_usage": true}` adds a final usage chunk |
| `n` | Partial | Only `n=1`; any other value returns 400 `n must be 1: one choice per request` |
| `seed` | Yes | The same seed, prompt and parameters give the same output on the same model |
| `presence_penalty` / `frequency_penalty` | Yes | -2.0 - 2.0 |
| `response_format` | Yes | `{"type": "json_object"}` constrains the answer to a JSON object; `{"type": "json_schema", "json_schema": {"name": …, "schema": {…}}}` to an object matching the schema. Output is compact JSON |
| `messages[].role` | Yes | `system`, `developer` (treated as `system`), `user`, `assistant` |
| `tools` / `functions` | **No** | Silently ignored |

---

## Streaming

SSE streaming is fully supported. Set `"stream": true` to receive token-by-token output:

```bash
curl -N http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}'
```

Each chunk is an SSE event:

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":10,"total_tokens":13}}

data: [DONE]
```

To display readable streaming output in terminal:

```bash
curl -sN http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}' \
  | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if line.startswith('data: ') and line != 'data: [DONE]':
        delta = json.loads(line[6:]).get('choices',[{}])[0].get('delta',{})
        print(delta.get('content',''), end='', flush=True)
print()
"
```

### Waiting for a worker

A streaming request can wait before its first token: while the providers' GPUs
are busy with a Filecoin proof, or while a worker loads the requested model
(tens of seconds for a large model). If nothing has been sent after **5
seconds**, the gateway starts the response — `200`, `Content-Type:
text/event-stream` — and sends an SSE comment line every 5 seconds until the
model's stream begins:

```
: waiting for a worker

: waiting for a worker

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
```

SSE parsers (the OpenAI SDKs, `EventSource`, the example above) skip comment
lines, so the stream reads the same. The comments keep proxies and browsers
from closing a connection that would otherwise carry no bytes for a minute.

Because the status line has already been sent, an error that ends such a wait
arrives inside the stream as an error event carrying the HTTP status it would
have had, followed by `[DONE]`:

```
data: {"error":{"message":"no available worker — all workers are mining or offline","type":"service_unavailable","status":503}}

data: [DONE]
```

Treat an error event with `status` 503 or 502 that arrives before any content
like an HTTP 503: retry after a short wait. A request answered within the first
5 seconds, or refused within them, gets its ordinary status and body.

### Streaming receipts (opt-in)

Non-streaming responses carry the worker-signed billing receipt in the
`X-Om-Receipt` header (settlement-api.md §2.2). A streaming response cannot use a
trailer, so the receipt arrives as a dedicated SSE event instead — but only when
the client asks for it, by sending the header:

```
X-OM-Receipt-Req: 1
```

The gateway then forwards one extra event before `[DONE]`:

```
data: {"om_receipt": {"v":1, "request_id":"…", "model":"…", "request_sha256":"…", "response_sha256":"…", "prompt_tokens":…, "completion_tokens":…, "cached_tokens":…, "ts":…, "pubkey":"…", "sig":"…"}}
```

Without the header the event is stripped and the stream is byte-identical to
plain OpenAI SSE — existing clients parse nothing new. Verification of the
receipt (five offline checks against the on-chain batch) is described in
settlement-api.md §2.2.

### Thinking mode (opt-in)

Reasoning-capable models (Qwen3-8B, Qwen3-32B-AWQ, Qwen3.8-27B-FP8,
openai/gpt-oss-20b) can think before answering. Off by default — answers,
latency and bills stay unchanged unless a request opts in:

```json
{"model": "Qwen/Qwen3.8-27B-FP8", "enable_thinking": true, "messages": [...]}
```

(`"chat_template_kwargs": {"enable_thinking": true}` is accepted as the
OpenAI-ecosystem spelling of the same switch. Models without a thinking
template silently ignore both.)

With thinking on, the reasoning arrives separated from the answer, matching the
convention used across OpenAI-compatible reasoning APIs:

- **Non-streaming**: `choices[0].message.reasoning_content` carries the chain
  of thought; `content` stays clean.
- **Streaming**: deltas carry `reasoning_content` first, then `content`.

Billing: reasoning tokens are generated tokens — they are billed as output at
the model's normal rate, and a thinking answer can cost several times a direct
one. The signed receipt hashes the raw generated text (reasoning included), so
verifiability is unchanged.

**One case surfaces `reasoning_content` even with thinking off.** `gpt-oss-20b`
reasons on every request — its template has no off switch — so by default that
reasoning is withheld and only the answer is returned. If `max_tokens` runs out
*before* any answer text exists, withholding it too would return an entirely
empty response for tokens you are billed for. Instead the reasoning is
returned in `reasoning_content` with `content: ""` and `finish_reason:
"length"` — read that pair as "raise `max_tokens`" (256 is a sane floor for
reasoning models). Requests that do produce an answer are unaffected: the
reasoning stays hidden unless you asked for it.

---

## Model Names

The following model names all route to the same backend:

| Model Name | Example |
|-----------|---------|
| `default` | **Not accepted** — the gateway returns 400 (`a specific model id is required`); pick a name from `GET /v1/models` |
| Full HuggingFace ID | `Qwen/Qwen3-4B-Instruct-2507` |
| Local path | `/models/Qwen--Qwen3-4B-Instruct-2507` |

A missing or `"default"` model returns **400** (the request must name a model); a named model that no worker has loaded or supports returns **404**:

```json
{"error":{"message":"no worker supports model \"gpt-nonexistent\"","type":"gateway_error"}}
```

The authoritative list of available models is `GET /v1/catalog` (see
settlement-api.md).

---

## Client Integration Examples

Complete programs that combine several modalities — a voice assistant, an
image look-edit-check loop, a narrated picture book and a billing verifier —
are in [`examples/`](../examples/README.md).

### Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://<host>:3000/v1",
    api_key="<CLIENT_TOKEN>"
)

# Non-streaming
response = client.chat.completions.create(
    model="Qwen/Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
)
print(response.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(
    model="Qwen/Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
    stream=True,
):
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### Python (LangChain)

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    base_url="http://<host>:3000/v1",
    api_key="<CLIENT_TOKEN>",
    model="Qwen/Qwen3-4B-Instruct-2507",
    max_tokens=200,
)

response = llm.invoke("Explain Filecoin in simple terms.")
print(response.content)
```

### cURL

```bash
curl http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hi"}],"max_tokens":50}'
```

### JavaScript (fetch)

```javascript
const response = await fetch("http://<host>:3000/v1/chat/completions", {
  method: "POST",
  headers: {
    "Authorization": "Bearer <CLIENT_TOKEN>",
    "Content-Type": "application/json"
  },
  body: JSON.stringify({
    model: "Qwen/Qwen3-4B-Instruct-2507",
    messages: [{ role: "user", content: "Hello" }],
    max_tokens: 100
  })
});
const data = await response.json();
console.log(data.choices[0].message.content);
```

---

## Token Usage

Token counts (`usage.prompt_tokens`, `usage.completion_tokens`) are computed by the vLLM inference engine from actual tokenized input/output. Values are accurate and suitable for billing. For `vision` models, images are part of the prompt: their visual tokens are included in `usage.prompt_tokens`.

The image and audio endpoints report their own billed quantity instead:
`usage.images` (image generation), `usage.seconds` (transcription), and — for
speech, whose body is audio — the receipt's `quantity` (see "Receipts").

`finish_reason` is accurate: `"stop"` when the model finishes naturally, `"length"` when truncated by `max_tokens`.

---

## Model-Aware Routing

When Workers are registered with `supported_models`, the gateway routes requests based on the `model` field:

1. **Priority 1**: Route to a Worker that already has the model loaded (no switch, lowest latency)
2. **Priority 2**: Route to an idle Worker that lists the model in `supported_models` (triggers model switch)
3. **Priority 3** (internal only): requests without a usable model preference — e.g. gateway-initiated stream-resume continuations — route to any available Worker. Client requests can never reach this branch: a missing or `"default"` model is rejected with 400 at the door

When all Workers that have the requested model loaded are overloaded (active requests exceed `gpu_count * model_switch_load_factor`), the gateway automatically routes to an idle Worker that supports the model, triggering a model switch to distribute load. This is configurable; set `model_switch_load_factor: 0` to disable.

**Mining-window awareness**: within the model-priority rules above, the gateway
**soft-de-prioritizes** workers **about to yield to mining** — the scheduler's `/ready`
emits `seconds_until_change` (seconds until the on-chain WindowPoSt graceful yield; omitted
when the scheduler has no estimate, which never counts as "about to yield"), and a
worker inside the 60s yield window has its weight multiplied by 0.05 (decaying with the
estimate). This is a soft preference, not a hard exclusion: when all workers are about to
yield they are still served (transparent stream resume is the backstop). The goal is to avoid handing a
long stream to a machine about to mine.

### Session affinity (prefix-cache reuse)

Send `X-Session-Id: <your-session-id>` and the gateway **prefers to stick subsequent
requests of that session to the same worker**, reusing vLLM's prefix cache (a multi-turn
conversation's shared prefix isn't recomputed — faster and cheaper). Affinity is a
**preference, not a pin**: if the sticky worker is gone / busy elsewhere / switched
models, it transparently falls back to normal weighted routing, never blocking the
request. The response header `X-Session-Key` returns an opaque fingerprint of the session
key (so isolation is observable: same key+session-id → same value; different api keys with
the same session-id → different values).

If no Worker supports the requested model:

```json
{
  "error": {
    "message": "no worker supports model \"llama-70b\"",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 404

The gateway automatically normalizes model name formats. All of these match the same model:
- `Qwen/Qwen3-4B-Instruct-2507` (HuggingFace ID)
- `/models/Qwen--Qwen3-4B-Instruct-2507` (local path)
- `Qwen--Qwen3-4B-Instruct-2507` (basename)

---

## Error Handling

### During Mining (All Workers Unavailable)

When all Workers are mining or loading models, requests are queued for up to `queue_timeout_sec` (default 60s, configurable to longer). If no Worker recovers within the timeout:

```json
{
  "error": {
    "message": "no available worker — all workers are mining or offline",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 503. The `Retry-After` header is an **honest resume estimate**: the
smallest resume-estimate (seconds) among the currently-mining workers — derived by the
scheduler from the on-chain WindowPoSt deadline and exposed on `/ready` — clamped to
[5,120]; it falls back to a fixed 30 when no estimate is available. Clients can retry at
the real recovery time instead of blindly waiting 30s.

If the queue is full (`max_queue_size` exceeded):

```json
{
  "error": {
    "message": "request rejected — queue full (100/100)",
    "type": "gateway_error"
  }
}
```

### Automatic Failover (Non-Streaming)

With 2+ Workers, if a Worker returns 503 mid-generation (e.g., WinningPoSt interrupts), the gateway automatically retries on a different Worker (up to 3 attempts). This is completely transparent to the client.

### Streaming interruption & transparent resume

If mining starts mid-stream and the gateway has `stream_resume` on (recommended), the
gateway **transparently continues the stream on another worker that advertises resume
support**: it re-feeds the already-delivered text as a prefix (`om_continuation`) plus the
remaining `max_tokens` to a new worker and keeps streaming — **the client sees no error
event** and still receives `data: [DONE]` normally. The continuation segment is billed by
the gateway's own count; the re-fed prefix is never double-billed. A single stream may
resume multiple times (bounded by a budget).

Only when a stream **cannot** be resumed (no capable worker / resume budget exhausted)
does it degrade to the legacy behavior — an error event and no `[DONE]`:

```
data: {"error": {"message": "Engine paused during generation — request aborted", "type": "server_error"}}
```

With `stream_resume` off (the default) it is always the legacy behavior. Separately, when a
client **abandons** a stream, it is billed for the tokens delivered before the disconnect
(see the billing rules in settlement-api.md).

### Model required (400)

A request without a `model`, or with the retired `"default"` alias, is rejected
before routing:

```json
{"error":{"message":"a specific model id is required (the \"default\" alias is no longer accepted); call GET /v1/models for the live list","type":"gateway_error"}}
```

### Request refused by the worker (4xx), streaming or not

A worker that rejects a request (for example an image sent to a text-only model)
answers with the usual JSON error and its 4xx status — also when the request asked for
`"stream": true`. No SSE events are sent in that case, so check the status code before
reading the body as a stream:

```json
{"error":{"message":"model '/models/Qwen--Qwen3-4B-Instruct-2507' does not accept image input","type":"upstream_error"}}
```

### Gateway starting or unable to record usage (503)

Two short-lived 503s carry a `Retry-After` header and are safe to retry:

- `gateway is starting (loading balances), please retry` — right after a gateway
  restart, until wallet balances are loaded (typically well under a minute).
- `gateway cannot record usage right now, please retry later` — the gateway cannot
  write its usage ledger (for example its data disk is full). Billable requests are
  refused rather than served unrecorded; nothing is reserved or charged.

### Upstream failure (502)

The serving worker failed mid-request. Non-streaming requests are retried on
another worker automatically before you ever see a 502 — a surfaced one means
retries were exhausted; retry later. For streams, see "Streaming interruption &
transparent resume" above.

### Unsupported Endpoints

```json
{
  "error": {
    "message": "endpoint /v1/embeddings is not supported",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 404. Unsupported endpoints include `/v1/embeddings`,
`/v1/images/edits` and `/v1/images/variations`.

### Image and audio endpoint errors

The image and audio endpoints share the authentication, balance gate, rate
limits and body cap of the chat endpoint (401, 402, 413, 429 as in
settlement-api.md §2.1). Their own errors:

| Status | When | `error.message` |
|---|---|---|
| 400 | Model of another kind sent to this endpoint | `model "/models/stabilityai--stable-diffusion-xl-base-1.0" is an image-generation model; use /v1/images/generations` |
| 400 | Missing `model` or the `"default"` alias | `a specific model id is required; call GET /v1/models to list available models` |
| 400 | Image `size` out of range or malformed | `size must be "WxH" with each side a multiple of 8 between 256 and 1024, or "auto" (got 'abc')` |
| 400 | `n` above the model's cap | `too many images: 4 requested, at most 2` |
| 400 | Audio upload in a format that is not accepted | `unsupported audio format: upload WAV, FLAC, MP3, OGG (Opus/Vorbis), WebM or M4A/MP4` |
| 400 | Compressed WAV encoding (e.g. ADPCM) | `WAV encoding N is compressed; upload PCM WAV or FLAC` |
| 400 | Audio whose container declares no length | e.g. `MP4 has no moov box (upload a complete file)`, `FLAC does not declare its length; re-encode with a known sample count` |
| 400 | Audio that plays longer than its container declares | `audio plays Xs but its container declares Ys; re-encode the file` |
| 400 | Transcription `response_format` not accepted | `response_format must be json, text, verbose_json, srt or vtt` |
| 400 | Transcription request not multipart / no `file` part | `expected multipart/form-data with an audio file` / `missing audio file field "file"` |
| 400 | Audio longer than `max_audio_sec` | `transcription too large: N audio_second, at most M` |
| 400 | Speech `response_format` not accepted | `response_format must be wav, mp3, opus, aac, flac or pcm` |
| 400 | Speech `voice` not available | `unknown voice 'x'; available: …` |
| 400 | Speech `speed` outside 0.25–4.0 | `speed must be between 0.25 and 4.0` |
| 400 | Image `response_format` other than `b64_json` / `url` | `response_format must be "b64_json" or "url"` |
| 400 | Image `output_format` not accepted | `output_format must be png, jpeg or webp` |
| 404 | Hosted image link expired (after one hour) or unknown | `image not found or expired` |
| 400 | Empty speech `input` | `input text is required` |
| 400 | Speech `input` longer than `max_chars` | `speech too large: N character, at most M` |
| 404 | No worker supports the named model | `no worker supports model "acme/none"` |
| 413 | Transcription upload above `max_audio_upload_bytes` (25 MiB default); other bodies above `max_request_bytes` | `request body exceeds limit of N bytes` |
| 503 | No worker serving this model is available within the queue window | `no worker available for this model` |

The 503 carries `Retry-After`: the soonest recovery estimate among the workers
that hold or support **that model**, clamped to [5, 120] seconds (falling back
to the network-wide estimate, or 30, when none of them has one). Like the
chat endpoint, a worker that yields mid-request is retried on another worker
before you ever see an error, and nothing is billed for a request that did
not deliver.

Rejected requests (any 4xx) are not billed.

---

## Management API (sp-gateway)

The gateway provides a REST API on port **9091** for Worker management.

**Authentication:** Bearer token (`AGENT_ADMIN_TOKEN`)

> **Operator-internal.** This admin API is for the gateway operator's own
> tooling; its token must never be handed out. A third-party Storage Provider
> joins through **self-registration** instead — the worker's scheduler signs a
> gateway challenge with the miner's own key and receives its credentials and
> TLS certificate automatically. That flow needs nothing from this section; see
> GETTING-STARTED-SP.md in the [openmodel](https://github.com/6block/openmodel)
> repository.

### Register a Worker

```bash
curl -X POST http://<host>:9091/api/v1/workers/register \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "sp-my-worker",
    "endpoint": "http://<worker-ip>:8000",
    "scheduler_url": "http://<worker-ip>:9090",
    "gpu_count": 1,
    "miner_address": "t0182063",
    "supported_models": ["Qwen/Qwen3-4B-Instruct-2507", "Qwen/Qwen3-8B"],
    "auth_token": "<secret the worker stack validates>"
  }'
```

Worker ID rules: 1-64 characters, starting alphanumeric, then alphanumeric plus `-`, `_`, `.`

Notes:
- `gpu_count` is an initial value. After the first poll, it is automatically updated from the Worker's actual `engine_count` to reflect true inference capacity.
- `supported_models` is optional but strongly recommended: the gateway routes model-aware, and a worker with no claimed (or loaded) models is never matched to client requests.
- `miner_address` is the Worker's miner address (mapped to an EVM payout address via `sp_address_map` at settlement).
- `auth_token` is optional, a **per-worker auth secret**: the gateway attaches it when forwarding inference requests, and the worker stack validates that requests really came from this gateway (preventing free-riding by connecting to the Worker directly, bypassing the gateway). It **must survive restarts** — if a poll gets a 401, the gateway marks that Worker offline.
- `loaded_model` is auto-detected from the Worker's `/health` endpoint — no need to specify.

### List Workers

```bash
curl http://<host>:9091/api/v1/workers \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Deregister a Worker

```bash
curl -X DELETE http://<host>:9091/api/v1/workers/<worker-id> \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Fleet Statistics

```bash
curl http://<host>:9091/api/v1/stats \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Health / Readiness

```bash
# Liveness (no auth required)
curl http://<host>:9091/health

# Readiness — checks if any workers can serve requests (no auth required)
curl http://<host>:9091/ready
```

### Prometheus Metrics

```bash
curl http://<host>:9091/metrics
```

Key metrics:
- `openmodel_workers_total{state}` — Worker count by state (idle, busy, mining, loading, offline)
- `openmodel_polls_total{result}` — Poll success/failure count
- `openmodel_poll_duration_seconds` — Poll latency histogram
- `openmodel_gateway_requests_total{status,worker_id}` — Gateway request count
- `openmodel_gateway_request_duration_seconds{worker_id}` — Request latency histogram
- `openmodel_gateway_queued_requests` — Currently queued requests
- `openmodel_worker_state_transitions_total{from,to}` — State transition counter
