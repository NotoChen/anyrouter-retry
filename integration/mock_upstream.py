#!/usr/bin/env python3
"""Local dummy Responses upstream; no external credentials or user sessions."""
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
state = dict(mode="sse", failures=110, calls=0, delay=0.0, fragment=False, post_output_delay=0.0, output_bytes=0)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def json_response(self, status, value):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        with lock:
            snapshot = dict(state)
        self.json_response(200, snapshot)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        if self.path == "/control":
            with lock:
                state.update(body)
                state["calls"] = 0
            self.json_response(200, {"ok": True})
            return
        with lock:
            state["calls"] += 1
            n = state["calls"]
            config = dict(state)
        mode = config["mode"]
        failure = n <= config["failures"]
        err = {"type": "too_many_requests", "code": "rate_limit_reached", "message": "dummy capacity limit"}
        if mode == "signature":
            self.json_response(400, {"error": {"type": "invalid_request_error", "code": "thinking_signature_invalid", "message": "dummy invalid signature"}})
            return
        if mode.startswith("http") and failure:
            self.json_response(int(mode[4:]), {"error": err})
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True

        def event(value):
            # Match real AnyRouter's event:/data: framing. CPA passes these
            # fields to plugins as separate chunks without newline delimiters.
            raw = ("event: " + value["type"] + "\ndata: " + json.dumps(value) + "\n\n").encode()
            try:
                if config["fragment"]:
                    for i in range(0, len(raw), 7):
                        self.wfile.write(raw[i:i + 7])
                        self.wfile.flush()
                else:
                    self.wfile.write(raw)
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                return False
            return True

        response = {"id": f"resp_{n}", "object": "response", "created_at": int(time.time()), "status": "in_progress", "model": "gpt-6-astra", "output": []}
        if not event({"type": "response.created", "response": response}):
            return
        time.sleep(config["delay"])
        if mode == "partial" and failure:
            event({"type": "response.output_text.delta", "delta": "partial", "output_index": 0, "content_index": 0})
        if failure:
            event({"type": "error", "error": err})
            return
        item = {"type": "message", "id": f"msg_{n}", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "RETRY_OK", "annotations": []}]}
        event({"type": "response.output_item.added", "output_index": 0, "item": {**item, "status": "in_progress", "content": []}})
        with lock:
            state["delta_sent_at"] = time.time()
        event({"type": "response.output_text.delta", "delta": "RETRY_OK"+"x"*config.get("output_bytes",0), "output_index": 0, "content_index": 0, "item_id": item["id"]})
        time.sleep(config["post_output_delay"])
        event({"type": "response.output_item.done", "output_index": 0, "item": item})
        event({"type": "response.completed", "response": {**response, "status": "completed", "output": [item], "usage": {"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 19997), Handler).serve_forever()
