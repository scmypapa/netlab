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
    requests, total, failures, latency, errors, by_target = 0, 0, 0, [], {}, {}
    while time.monotonic() < deadline:
        target = targets[(index + requests + failures) % len(targets)]
        measured = by_target.setdefault(target, {"requests": 0, "failures": 0, "bytes": 0})
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
                measured["requests"] += 1
                measured["bytes"] += count
                latency.append((time.monotonic() - before) * 1000)
        except Exception as error:
            failures += 1
            measured["failures"] += 1
            message = str(error)
            errors[message] = errors.get(message, 0) + 1
    return requests, total, failures, latency, errors, by_target


with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
    values = list(pool.map(load, range(concurrency)))
elapsed = time.monotonic() - started
latency = sorted(value for result in values for value in result[3])
errors, by_target = {}, {}
for result in values:
    for message, count in result[4].items():
        errors[message] = errors.get(message, 0) + count
    for target, measured in result[5].items():
        aggregate = by_target.setdefault(target, {"requests": 0, "failures": 0, "bytes": 0})
        for field, count in measured.items():
            aggregate[field] += count
total = sum(result[1] for result in values)
requests = sum(result[0] for result in values)
print(json.dumps({"durationSeconds": elapsed, "concurrency": concurrency,
                  "requests": requests, "failures": sum(result[2] for result in values),
                  "bytes": total, "requestsPerSecond": requests / elapsed,
                  "MiBPerSecond": total / elapsed / 1048576,
                  "p95Ms": latency[max(0, (95 * len(latency) + 99) // 100 - 1)] if latency else None,
                  "errors": errors, "targets": by_target}))
