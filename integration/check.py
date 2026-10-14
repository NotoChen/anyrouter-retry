#!/usr/bin/env python3
"""End-to-end checks through the real dynamically loaded CPA plugin."""
import json
import time
import urllib.request

urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))


def control(**options):
    options = {"delay": 0, "fragment": False, "post_output_delay": 0, "output_bytes": 0, **options}
    req = urllib.request.Request("http://127.0.0.1:19997/control", json.dumps(options).encode(), {"Content-Type": "application/json"})
    urllib.request.urlopen(req).read()


def calls():
    return json.load(urllib.request.urlopen("http://127.0.0.1:19997/stats"))["calls"]


def request():
    req = urllib.request.Request("http://127.0.0.1:19998/v1/responses", json.dumps({"model": "gpt-6-astra", "stream": True, "input": "say RETRY_OK"}).encode(), {"Content-Type": "application/json", "Authorization": "Bearer dummy-client"})
    return urllib.request.urlopen(req, timeout=15)


def check_success(mode, failures):
    control(mode=mode, failures=failures)
    start = time.monotonic()
    with request() as resp:
        payload = resp.read().decode()
    assert calls() == failures + 1, (mode, calls(), payload)
    assert payload.count('"type": "response.created"') + payload.count('"type":"response.created"') == 1, payload
    assert '"response.completed"' in payload and "RETRY_OK" in payload, payload
    assert "rate_limit_reached" not in payload, payload
    print(json.dumps(dict(test=mode, attempts=calls(), client_requests=1, completed=True, seconds=round(time.monotonic() - start, 3))))


if __name__ == "__main__":
    check_success("sse", 110)
    for status in [429, 500, 502, 503, 504, 524]:
        check_success("http" + str(status), 2)
    control(mode="signature", failures=1)
    with request() as resp:
        payload = resp.read().decode()
    assert calls() == 1 and "thinking_signature_invalid" in payload and "response.completed" not in payload, payload
    print(json.dumps(dict(test="invalid-history", attempts=1, retried=False)))
    control(mode="partial", failures=3)
    with request() as resp:
        payload = resp.read().decode()
    assert calls() == 1 and "partial" in payload and "response.completed" not in payload, payload
    print(json.dumps(dict(test="partial-output", attempts=1, retried=False)))
    control(mode="sse", failures=0)
    req = urllib.request.Request("http://127.0.0.1:19997/control", json.dumps(dict(mode="sse", failures=0, calls=0, delay=1.3, fragment=True)).encode(), {"Content-Type": "application/json"})
    urllib.request.urlopen(req).read()
    with request() as resp:
        payload = resp.read().decode()
    assert payload.count("keepalive") >= 3 and "response.completed" in payload, payload
    print(json.dumps(dict(test="delayed-fragmented-stream", heartbeats=payload.count("keepalive"), completed=True)))
