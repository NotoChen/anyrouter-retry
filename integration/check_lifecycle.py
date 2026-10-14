#!/usr/bin/env python3
"""Exercise graceful switches through actual CPA management and model APIs."""
import concurrent.futures
import json
from pathlib import Path
import threading
import time
import urllib.request

from check import calls, control, request

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(path, body=None):
    req = urllib.request.Request("http://127.0.0.1:19998" + path, None if body is None else json.dumps(body).encode(), {"Authorization": "Bearer dummy-management", "Content-Type": "application/json"}, method="GET" if body is None else "PATCH")
    return json.load(opener.open(req, timeout=10))


def state():
    return api("/v0/management/plugins/anyrouter-retry/runtime")


def switch(accept):
    api("/v0/management/plugins/anyrouter-retry/config", {"accept_new_requests": accept})
    deadline = time.monotonic() + 5
    while state()["accept_new_requests"] != accept:
        assert time.monotonic() < deadline, "switch not applied"
        time.sleep(0.02)


def read_request(output_seen=None):
    chunks = []
    with request() as response:
        for line in response:
            chunks.append(line)
            if output_seen and line.startswith(b"data:"):
                event = json.loads(line[5:])
                if event.get("type") == "response.output_text.delta":
                    output_seen.set()
    return b"".join(chunks).decode()


def wait_calls(n):
    deadline = time.monotonic() + 5
    while calls() < n:
        assert time.monotonic() < deadline, "upstream not called"
        time.sleep(0.02)


def success(payload, plugin):
    assert payload.count('"type": "response.completed"') + payload.count('"type":"response.completed"') == 1, payload
    assert "RETRY_OK" in payload and "rate_limit_reached" not in payload, payload
    assert ("keepalive" in payload) == plugin, payload


if __name__ == "__main__":
    switch(True)
    with concurrent.futures.ThreadPoolExecutor() as pool:
        control(mode="sse", failures=2, delay=1.0, post_output_delay=0)
        old = pool.submit(read_request)
        wait_calls(1)
        assert state()["active_requests_current_instance"] == 1
        switch(False)
        native_failure = pool.submit(read_request)
        wait_calls(2)
        failed = native_failure.result(timeout=10)
        assert "keepalive" not in failed and "rate_limit_reached" in failed, failed
        success(old.result(timeout=10), plugin=True)
        print(json.dumps({"test": "pause-during-bootstrap-retries", "old_request_completed": True, "new_request_native": True, "upstream_calls": calls()}))

        control(mode="sse", failures=0, delay=1.0, post_output_delay=0)
        native = pool.submit(read_request)
        wait_calls(1)
        switch(True)
        new = pool.submit(read_request)
        success(native.result(timeout=10), plugin=False)
        success(new.result(timeout=10), plugin=True)
        print(json.dumps({"test": "enable-while-native-request-waits", "native_request_preserved": True, "new_request_plugin": True}))

        control(mode="sse", failures=0, delay=0, post_output_delay=1.0)
        output_seen = threading.Event()
        streaming = pool.submit(read_request, output_seen)
        assert output_seen.wait(5), "no output before pause"
        switch(False)
        assert state()["active_requests_current_instance"] == 1
        success(streaming.result(timeout=10), plugin=True)
        print(json.dumps({"test": "pause-after-output", "same_stream_completed": True, "replayed": False}))

        switch(True)
        control(mode="sse", failures=0, delay=1.0, post_output_delay=0)
        waiting = pool.submit(read_request)
        wait_calls(1)
        for _ in range(5):
            switch(False)
            switch(True)
        success(waiting.result(timeout=10), plugin=True)
        assert calls() == 1, "switching replayed a successful request"
        print(json.dumps({"test": "ten-switches-during-request", "client_request_completed": True, "upstream_calls": 1}))

    switch(True)
    control(mode="sse", failures=0, delay=0, post_output_delay=0)
