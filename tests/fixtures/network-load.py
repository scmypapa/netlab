import concurrent.futures
import json
import math
import sys
import time
import urllib.request

targets = json.loads(sys.argv[1])
concurrency = int(sys.argv[2])
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def access(target):
    started = time.perf_counter()
    with opener.open(target["url"], timeout=5) as response:
        body = response.read()
        if response.status != 200:
            raise AssertionError(f"HTTP {response.status}: {target['url']}")
        if "body" in target and body.decode() != target["body"]:
            raise AssertionError(f"环境隔离错误: {target['url']}")
        if "size" in target and len(body) != target["size"]:
            raise AssertionError(f"数据长度错误: {target['url']}: {len(body)}")
    return (time.perf_counter() - started) * 1000, len(body)


with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
    results = list(pool.map(access, targets))
latencies = sorted(item[0] for item in results)
print(json.dumps({
    "requests": len(results),
    "concurrency": concurrency,
    "bytes": sum(item[1] for item in results),
    "p50Ms": round(latencies[(len(latencies) - 1) // 2], 3),
    "p95Ms": round(latencies[math.ceil(len(latencies) * 0.95) - 1], 3),
    "maxMs": round(latencies[-1], 3),
}))
