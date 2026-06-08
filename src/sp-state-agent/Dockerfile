FROM golang:1.24-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /sp-state-agent cmd/agent/main.go

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /sp-state-agent /app/sp-state-agent

RUN mkdir -p /data

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:9091/health || exit 1

ENTRYPOINT ["/app/sp-state-agent"]
CMD ["-config", "/etc/openmodel/sp-state-agent.yaml"]
