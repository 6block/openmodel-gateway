# OpenModel M2 — API Gateway Deployment Guide

## Deliverables

```
openmodel-m2/
├── README.md                      # This document
├── docker-compose.m2.yml          # Compose file (pre-built image)
├── .env.example                   # Environment variable template
├── config/
│   └── sp-state-agent.yaml        # Gateway configuration
└── docs/
    └── m2-api-docs.md             # API integration guide

└── images/
    └── openmodel-sp-gateway.tar.gz      # Gateway Docker image (~11MB)
```

## 1. Prerequisites

| Item | Requirement |
|------|-------------|
| OS | Ubuntu 22.04+ or compatible Linux distribution |
| Docker | 24+ with Docker Compose v2 |
| M1 Stack | go-scheduler (:9090) + py-inference (:8000) running on each SP Worker |
| Network | Coordinator must reach each Worker's ports 8000 and 9090 |

M2 runs on a coordinator machine. SP Workers run the existing M1 stack unchanged — no M2 components needed on Worker machines.

## 2. Load Image

```bash
docker load -i images/openmodel-sp-gateway.tar.gz
```

## 3. Configure

```bash
# Create data directory
mkdir -p data/state-agent

# Copy the template
cp .env.example .env.m2

# Generate tokens
CLIENT_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 40)
AGENT_ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 40)

cat > .env.m2 <<EOF
CLIENT_TOKEN=$CLIENT_TOKEN
AGENT_ADMIN_TOKEN=$AGENT_ADMIN_TOKEN
EOF
chmod 600 .env.m2

echo "Client token: $CLIENT_TOKEN"
echo "Admin token: $AGENT_ADMIN_TOKEN"
```

## 4. Start Gateway

```bash
set -a && source .env.m2 && set +a
docker compose -f docker-compose.m2.yml up -d
```

Verify:
```bash
curl http://localhost:9091/health
# {"status":"ok"}
```

## 5. Register SP Workers

For each SP Worker machine:

```bash
curl -X POST http://localhost:9091/api/v1/workers/register \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "sp-worker-1",
    "endpoint": "http://<worker-ip>:8000",
    "scheduler_url": "http://<worker-ip>:9090",
    "gpu_count": 1,
    "miner_address": "<filecoin-miner-address>",
    "supported_models": ["Qwen/Qwen2.5-3B-Instruct"]
  }'
```

## 6. Test

```bash
# Inference
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"Hello"}],"max_tokens":20}'

# Worker status
curl http://localhost:9091/api/v1/workers \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

See `docs/m2-api-docs.md` for full API reference.
