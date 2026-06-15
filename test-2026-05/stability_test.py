"""Long-running stability test — continuous inference requests with periodic stats."""
import concurrent.futures
import json
import time
import urllib.request
import urllib.error
import sys
import threading

GATEWAY_URL = "http://localhost:3000/v1/chat/completions"
TOKEN = sys.argv[1] if len(sys.argv) > 1 else ""
CONCURRENCY = 3
REPORT_INTERVAL = 60  # Print stats every 60s

PAYLOAD = json.dumps({
    "model": "default",
    "messages": [{"role": "user", "content": "Explain blockchain in one sentence."}],
    "max_tokens": 30
}).encode()

# Shared counters (thread-safe via lock)
lock = threading.Lock()
stats = {
    "total": 0, "success": 0, "errors": 0,
    "status_codes": {},
    "latencies": [],
    "worker_counts": {},
}

def send_request():
    t0 = time.monotonic()
    try:
        req = urllib.request.Request(GATEWAY_URL, data=PAYLOAD, method="POST")
        req.add_header("Content-Type", "application/json")
        if TOKEN:
            req.add_header("Authorization", f"Bearer {TOKEN}")
        with urllib.request.urlopen(req, timeout=60) as resp:
            body = resp.read()
            elapsed = (time.monotonic() - t0) * 1000
            # Try to get worker from response header
            worker_id = resp.headers.get("X-Worker-ID", "unknown")
            return elapsed, resp.status, None
    except urllib.error.HTTPError as e:
        elapsed = (time.monotonic() - t0) * 1000
        return elapsed, e.code, str(e)
    except Exception as e:
        elapsed = (time.monotonic() - t0) * 1000
        return elapsed, 0, str(e)

def record(lat, status, err):
    with lock:
        stats["total"] += 1
        stats["status_codes"][status] = stats["status_codes"].get(status, 0) + 1
        stats["latencies"].append(lat)
        if err or status >= 500:
            stats["errors"] += 1
        else:
            stats["success"] += 1

def print_report(elapsed_min):
    with lock:
        total = stats["total"]
        success = stats["success"]
        errors = stats["errors"]
        codes = dict(sorted(stats["status_codes"].items()))
        lats = sorted(stats["latencies"])

    if not lats:
        return

    p50 = lats[len(lats) // 2]
    p95 = lats[int(len(lats) * 0.95)]
    p99 = lats[int(len(lats) * 0.99)]
    avg = sum(lats) / len(lats)
    rps = total / (elapsed_min * 60) if elapsed_min > 0 else 0

    print(f"[{elapsed_min:.0f}min] total={total} success={success} errors={errors} "
          f"rps={rps:.1f} P50={p50:.0f}ms P95={p95:.0f}ms P99={p99:.0f}ms "
          f"avg={avg:.0f}ms max={max(lats):.0f}ms codes={codes}",
          flush=True)

print(f"Stability test started: concurrency={CONCURRENCY}, reporting every {REPORT_INTERVAL}s")
print(f"Press Ctrl+C to stop\n", flush=True)

t_start = time.monotonic()
last_report = t_start
running = True

def worker_loop():
    while running:
        lat, status, err = send_request()
        record(lat, status, err)
        time.sleep(0.1)  # Small gap between requests

threads = []
for i in range(CONCURRENCY):
    t = threading.Thread(target=worker_loop, daemon=True)
    t.start()
    threads.append(t)

try:
    while True:
        time.sleep(5)
        now = time.monotonic()
        elapsed_min = (now - t_start) / 60
        if now - last_report >= REPORT_INTERVAL:
            print_report(elapsed_min)
            last_report = now
except KeyboardInterrupt:
    running = False
    elapsed_min = (time.monotonic() - t_start) / 60
    print(f"\n--- FINAL REPORT ({elapsed_min:.1f} minutes) ---")
    print_report(elapsed_min)
