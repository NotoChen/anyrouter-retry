#!/usr/bin/env python3
"""Install into the existing local CPA; preserve its other plugin/config text."""
import datetime
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent
CONFIG = Path.home() / "cli-proxy-api/config.yaml"


def main():
    original = CONFIG.read_text()
    if re.search(r"^    anyrouter-retry:", original, re.M):
        raise SystemExit("anyrouter-retry is already configured; update its existing block instead")
    if not (ROOT / "dist/anyrouter-retry-v0.1.12.so").is_file():
        raise SystemExit("Run ./build.sh first")
    match = re.search(r"^codex-api-key:\n(.*?)(?=^[^\s#]|\Z)", original, re.M | re.S)
    if not match:
        raise SystemExit("Installer requires this deployment's legacy codex-api-key layout")
    block = match.group(1)
    chunks = re.split(r"(?=^  - api-key:)", block, flags=re.M)
    changed = 0
    for i, item in enumerate(chunks):
        if not re.search(r"^    base-url: ['\"]?https://anyrouter\.top/v1['\"]?\s*$", item, re.M):
            continue
        if re.search(r"excluded-models:[^\n]*['\"]\*['\"]|^      - ['\"]\*['\"]", item, re.M):
            continue
        if "gpt-6-astra" not in item:
            continue
        if re.search(r"^    request-retry:", item, re.M):
            item = re.sub(r"^    request-retry:.*$", "    request-retry: 0", item, flags=re.M)
        else:
            item = re.sub(r"(^    base-url:.*\n)", r"\1    request-retry: 0\n", item, count=1, flags=re.M)
        chunks[i] = item
        changed += 1
    if changed != 4:
        raise SystemExit(f"Expected four currently eligible AnyRouter entries, found {changed}; config left intact")
    updated = original[:match.start(1)] + "".join(chunks) + original[match.end(1):]
    plugin = """    anyrouter-retry:
      enabled: true
      accept_new_requests: true
      config_path: /CLIProxyAPI/config.yaml
      upstream_hosts: [anyrouter.top]
      models: [gpt-6-astra]
      max_attempts: 0
      heartbeat_interval: 5s
      retry_delay: 0s
      max_bootstrap_bytes: 8388608
      max_sse_frame_bytes: 67108864
      trace_dir: /CLIProxyAPI/plugins/anyrouter-retry-data
      trace_retention_days: 7
      trace_max_bytes_per_request: 33554432
"""
    anchor = "  configs:\n    sub2api-balance:\n"
    if updated.count(anchor) != 1:
        raise SystemExit("Plugin configuration anchor changed; config left intact")
    updated = updated.replace(anchor, "  configs:\n" + plugin + "    sub2api-balance:\n", 1)
    backup_dir = ROOT / "backups"
    backup_dir.mkdir(mode=0o700, exist_ok=True)
    backup = backup_dir / (datetime.datetime.now().strftime("config-before-%Y%m%d-%H%M%S") + ".yaml")
    fd = os.open(backup, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "w") as file:
        file.write(original)
    subprocess.run(["docker", "cp", str(ROOT / "dist/anyrouter-retry-v0.1.12.so"), "cli-proxy-api:/CLIProxyAPI/plugins/linux/arm64/anyrouter-retry-v0.1.12.so"], check=True)
    # Write in place: replacing a singly bind-mounted config changes its inode
    # and leaves Docker reading the old file.
    with CONFIG.open("w") as file:
        file.write(updated)
        file.flush()
        os.fsync(file.fileno())
    print(f"Installed anyrouter-retry; native retries disabled on {changed} eligible AnyRouter keys")
    print(f"Config backup: {backup}")


if __name__ == "__main__":
    main()
