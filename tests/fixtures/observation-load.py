import concurrent.futures
import json
import sys
import time
import urllib.request

targets = json.loads(sys.argv[1])
concurrency = int(sys.argv[2])
duration = int(sys.argv[3])
started = time.monotonic()
deadline = started + duration


def load(index):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    requests, total, failures, latency, errors = 0, 0, 0, [], {}
    while time.monotonic() < deadline:
        target = targets[(index + requests + failures) % len(targets)]
        before = time.monotonic()
        try:
            with opener.open(target, timeout=10) as response:
                count = 0
                while chunk := response.read(65536):
                    count += len(chunk)
                if count != 2 * 1024 * 1024:
                    raise ValueError(f"body length {count}")
                total += count
                requests += 1
                latency.append((time.monotonic() - before) * 1000)
        except Exception as error:
            failures += 1
            message = str(error)
            errors[message] = errors.get(message, 0) + 1
    return requests, total, failures, latency, errors


with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
    values = list(pool.map(load, range(concurrency)))
elapsed = time.monotonic() - started
latency = sorted(value for result in values for value in result[3])
errors = {}
for result in values:
    for message, count in result[4].items():
        errors[message] = errors.get(message, 0) + count
total = sum(result[1] for result in values)
requests = sum(result[0] for result in values)
print(json.dumps({"durationSeconds": elapsed, "concurrency": concurrency,
                  "requests": requests, "failures": sum(result[2] for result in values),
                  "bytes": total, "requestsPerSecond": requests / elapsed,
                  "MiBPerSecond": total / elapsed / 1048576,
                  "p95Ms": latency[max(0, (95 * len(latency) + 99) // 100 - 1)] if latency else None,
                  "errors": errors}))
