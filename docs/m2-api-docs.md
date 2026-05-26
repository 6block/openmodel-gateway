# OpenModel M2 — API Integration Guide

## Overview

OpenModel M2 provides an **OpenAI-compatible API gateway** that routes inference requests across multiple Filecoin SP Workers. When a Worker begins mining (WindowPoSt/WinningPoSt), it is automatically removed from the routing pool, and requests are transparently redirected to available Workers.

**Base URL:** `http://<gateway-host>:3000`

**Authentication:** All requests require a Bearer token in the `Authorization` header.

```
Authorization: Bearer <CLIENT_TOKEN>
```

Each response includes an `X-Request-ID` header for request tracing.

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
    "model": "default",
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
  "model": "default",
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

Returns the list of available models across all active Workers.

---

## Supported Parameters

| Parameter | Supported | Notes |
|-----------|:---------:|-------|
| `model` | Yes | Use `"default"` or the full model name (e.g. `"Qwen/Qwen2.5-1.5B-Instruct"`) |
| `messages` | Yes | Standard chat message array |
| `max_tokens` | Yes | |
| `temperature` | Yes | 0.0 - 2.0 |
| `top_p` | Yes | 0.0 - 1.0 |
| `stop` | Yes | String or array of stop sequences |
| `stream` | Yes | SSE streaming supported |
| `n` | Partial | Only `n=1` is reliably supported |
| `tools` / `functions` | **No** | Silently ignored |
| `response_format` | **No** | `json_object` mode is not enforced |
| `seed` | Partial | Passed through but reproducibility is not guaranteed |

---

## Streaming

SSE streaming is fully supported. Set `"stream": true` to receive token-by-token output:

```bash
curl -N http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}'
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
  -d '{"model":"default","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}' \
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

---

## Model Names

The following model names all route to the same backend:

| Model Name | Example |
|-----------|---------|
| `default` | Recommended - always routes to whatever model is loaded |
| Full HuggingFace ID | `Qwen/Qwen2.5-1.5B-Instruct` |
| Local path | `/models/Qwen--Qwen2.5-1.5B-Instruct` |

Unknown model names are automatically rewritten to `"default"` to prevent unintended model switching.

---

## Client Integration Examples

### Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://<host>:3000/v1",
    api_key="<CLIENT_TOKEN>"
)

# Non-streaming
response = client.chat.completions.create(
    model="default",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
)
print(response.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(
    model="default",
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
    model="default",
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
  -d '{"model":"default","messages":[{"role":"user","content":"Hi"}],"max_tokens":50}'
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
    model: "default",
    messages: [{ role: "user", content: "Hello" }],
    max_tokens: 100
  })
});
const data = await response.json();
console.log(data.choices[0].message.content);
```

---

## Token Usage

Token counts (`usage.prompt_tokens`, `usage.completion_tokens`) are computed by the vLLM inference engine from actual tokenized input/output. Values are accurate and suitable for billing.

`finish_reason` is accurate: `"stop"` when the model finishes naturally, `"length"` when truncated by `max_tokens`.

---

## Model-Aware Routing

When Workers are registered with `supported_models`, the gateway routes requests based on the `model` field:

1. **Priority 1**: Route to a Worker that already has the model loaded (no switch, lowest latency)
2. **Priority 2**: Route to an idle Worker that lists the model in `supported_models` (triggers model switch)
3. **Priority 3**: For `model: "default"`, route to any available Worker

When all Workers that have the requested model loaded are overloaded (active requests exceed `gpu_count * model_switch_load_factor`), the gateway automatically routes to an idle Worker that supports the model, triggering a model switch to distribute load. This is configurable; set `model_switch_load_factor: 0` to disable.

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
- `Qwen/Qwen2.5-3B-Instruct` (HuggingFace ID)
- `/models/Qwen--Qwen2.5-3B-Instruct` (local path)
- `Qwen--Qwen2.5-3B-Instruct` (basename)

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

**HTTP Status:** 503 with `Retry-After: 30` header.

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

### Streaming Interruption

If mining starts during a streaming response, the client receives partial content followed by an error event:

```
data: {"error": {"message": "Engine paused during generation — request aborted", "type": "server_error"}}
```

No `data: [DONE]` is sent. The client should handle this gracefully.

### Unsupported Endpoints

```json
{
  "error": {
    "message": "endpoint /v1/embeddings is not supported",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 404. Unsupported endpoints: `/v1/embeddings`, `/v1/images/*`, `/v1/audio/*`.

---

## Management API (sp-gateway)

The gateway provides a REST API on port **9091** for Worker management.

**Authentication:** Bearer token (`AGENT_ADMIN_TOKEN`)

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
    "supported_models": ["Qwen/Qwen2.5-3B-Instruct", "Qwen/Qwen2.5-1.5B-Instruct"]
  }'
```

Worker ID rules: 1-64 characters, alphanumeric plus `-`, `_`, `.`

Notes:
- `gpu_count` is an initial value. After the first poll, it is automatically updated from the Worker's actual `engine_count` to reflect true inference capacity.
- `supported_models` is optional. If omitted, the Worker only accepts `model: "default"` requests. If provided, the gateway uses model-aware routing to match requests to Workers.
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
