# Public verification record

This file contains reproducible checks for the distributable repository. Machine-specific configuration backups, request traces, browser screenshots, and local session identifiers stay outside the public repository.

## v0.1.12

- `go test ./...` passed.
- `go test -race ./...` passed.
- The embedded dashboard JavaScript passed `node --check`.
- The plugin builds as a CPA C-ABI shared library with the repository build script.
- The integration test harness covers pre-output retry sequences, HTTP 408/425/429 and transient 5xx responses, SSE fragmentation, delayed streams, permanent request errors, output-after-retry boundaries, concurrent session identity, management authentication, lifecycle switching, and hot reload.
- The dashboard uses CPA Manager Plus's existing same-origin theme bridge and does not add a second theme controller.

## Runtime boundaries

The plugin retries only before it has forwarded generated content. It sends each attempt through CPA's `host.model.execute_stream`, keeps the client connection alive with SSE keepalive events, and exposes a successful attempt only after `response.completed` reaches the host stream. Once content, reasoning, or a tool item has been forwarded, it does not replay the request from the beginning.

`max_attempts: 0` means no plugin-side attempt count limit; cancellation, client disconnect, credential exhaustion, invalid request errors, or an explicit host shutdown can still end a request. `retry_delay: 0s` removes plugin-added delay but cannot remove the upstream request's own network or queue time.

## Reproduce locally

```sh
./build.sh
go test -race ./...
python3 -m py_compile control.py install.py sync_session_labels.py integration/*.py
```

The integration scripts require an isolated CPA test container and the local mock upstream described in `README.md`. They do not require production provider credentials.
