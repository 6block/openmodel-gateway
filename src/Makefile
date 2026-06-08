.PHONY: proto build build-linux test test-go test-py run dev deploy clean

# Server for deployment
SERVER ?= user@server
DEPLOY_PATH ?= /opt/openmodel

# Proto generation
proto:
	@chmod +x scripts/gen-proto.sh
	@scripts/gen-proto.sh

# Build Go scheduler
build:
	cd go-scheduler && go build -o bin/scheduler cmd/scheduler/main.go

# Cross-compile for Linux (deploy from macOS)
build-linux:
	cd go-scheduler && GOOS=linux GOARCH=amd64 go build -o bin/scheduler-linux cmd/scheduler/main.go

# Run all tests
# Note: the project currently has no unit tests; validation is done end-to-end
# via Docker deployment and live mining yield/resume scenarios. See:
#   docs/M1-milestone-report-en.md (Section 1: Testing & Validation)
test: test-go test-py

test-go:
	@cd go-scheduler && go test ./... || true

test-py:
	@if [ -d py-inference/tests ] && [ -n "$$(find py-inference/tests -name 'test_*.py' 2>/dev/null)" ]; then \
		cd py-inference && python -m pytest tests/ -v; \
	else \
		echo "No Python unit tests yet — see end-to-end validation in deployment guide."; \
	fi

# Run locally in dev mode
dev:
	@echo "Starting scheduler in dev mode..."
	cd go-scheduler && go run cmd/scheduler/main.go -config ../config/sidecar-prod-test.yaml

# Deploy to server via rsync
deploy: build-linux
	rsync -avz --progress \
		--exclude '.git' \
		--exclude '__pycache__' \
		--exclude '*.pyc' \
		--exclude 'go-scheduler/bin' \
		--exclude '.venv' \
		. $(SERVER):$(DEPLOY_PATH)/
	@echo "Copying Linux binary..."
	scp go-scheduler/bin/scheduler-linux $(SERVER):$(DEPLOY_PATH)/go-scheduler/bin/scheduler
	@echo "Deploy complete. Run on server:"
	@echo "  ssh $(SERVER) 'cd $(DEPLOY_PATH) && docker-compose up -d --build'"

# Docker — full stack containerization
docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-multi:
	SIDECAR_CONFIG=sidecar-8gpu-multi.yaml docker compose up -d --build

docker-tensor:
	SIDECAR_CONFIG=sidecar-8gpu-tensor.yaml docker compose up -d --build

docker-single:
	SIDECAR_CONFIG=sidecar-prod-test.yaml docker compose up -d --build

docker-logs:
	docker compose logs -f

docker-status:
	@docker compose ps
	@echo "---"
	@curl -s http://localhost:8000/health 2>/dev/null && echo "Inference: READY" || echo "Inference: NOT READY"
	@curl -s http://localhost:3100/health 2>/dev/null && echo "FOC Bridge: READY" || echo "FOC Bridge: NOT READY"

# Clean build artifacts
clean:
	rm -rf go-scheduler/bin/
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
