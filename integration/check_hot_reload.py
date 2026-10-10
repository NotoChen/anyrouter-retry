#!/usr/bin/env python3
"""Replace a real CPA plugin while a client is waiting for upstream success."""
import concurrent.futures
import json
from pathlib import Path
import shutil
import time
import urllib.request

from check import control, calls, request


def read_request():
    with request() as response:
        return response.read().decode()


if __name__ == "__main__":
    root = Path(__file__).resolve().parent.parent
    urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:19997/control", json.dumps(dict(mode="sse", failures=2, delay=1.3, fragment=False)).encode(), {"Content-Type": "application/json"})).read()
    with concurrent.futures.ThreadPoolExecutor() as pool:
        waiting = pool.submit(read_request)
        deadline = time.monotonic() + 5
        while calls() == 0:
            assert time.monotonic() < deadline, "upstream did not open"
            time.sleep(.05)
        # The isolated host selects filenames by version. This fixture is
        # identical corrected code with a higher test selection version.
        shutil.copy2(root / "dist/anyrouter-retry-v0.1.12.so", root / "dist/anyrouter-retry-v0.1.13.so")
        cfg = root / "integration/config.yaml"
        with cfg.open("a") as file:
            file.write("\n# hot reload while the downstream client remains open\n")
        payload = waiting.result(timeout=12)
    assert calls() == 3, calls()
    assert payload.count('"type":"response.created"') + payload.count('"type": "response.created"') == 1, payload
    assert "response.completed" in payload and "RETRY_OK" in payload and "rate_limit_reached" not in payload, payload
    print(json.dumps(dict(test="hot-reload-during-retries", upstream_attempts=calls(), client_requests=1, completed=True, old_stream_preserved=True)))
