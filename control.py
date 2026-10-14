#!/usr/bin/env python3
"""Graceful runtime switch. Never unload a plugin that owns client streams."""
import argparse
import http.client
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.request
import webbrowser


def main():
    parser = argparse.ArgumentParser(description="AnyRouter 重试插件无损启停：保留已接管请求，只改变后续请求的路由")
    parser.add_argument("action", choices=["on", "off", "status", "dashboard"])
    parser.add_argument("--url", default="http://127.0.0.1:9999")
    parser.add_argument("--key-env", default="CPA_MANAGEMENT_KEY", help="管理密钥的环境变量名；本机可自动读取 CPAMP 容器")
    parser.add_argument("--panel-url", default="http://127.0.0.1:19999", help="CPA Manager Plus 地址，用于打开插件菜单")
    args = parser.parse_args()
    if args.action == "dashboard":
        url = args.panel_url.rstrip("/") + "/management.html#/plugin-pages/anyrouter-retry/0"
        webbrowser.open(url)
        print("已打开 CPA Manager Plus 的 AnyRouter 会话请求菜单。")
        return
    key = os.environ.get(args.key_env)
    if not key:
        try:
            info = json.loads(subprocess.check_output(["docker", "inspect", "cpa-manager-plus"], stderr=subprocess.DEVNULL))[0]
            key = next(value.split("=", 1)[1] for value in info["Config"]["Env"] if value.startswith("CPA_MANAGEMENT_KEY="))
        except (subprocess.CalledProcessError, KeyError, StopIteration, FileNotFoundError):
            parser.error("请通过环境变量提供 CPA 管理密钥")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    base = args.url.rstrip("/") + "/v0/management/plugins"

    def call(path="", method="GET", data=None, runtime=False):
        body = None if data is None else json.dumps(data).encode()
        # Config/enabled and plugin-owned routes use CPA 8.0.22's v0
        # compatibility path. v8 only exposes selected plugin operations.
        request = urllib.request.Request(base + path, body, {"Authorization": "Bearer " + key, "Content-Type": "application/json"}, method=method)
        with opener.open(request, timeout=10) as response:
            return json.load(response)

    try:
        listing = call()
        records = listing.get("plugins", []) if isinstance(listing, dict) else listing
        plugin = next((item for item in records if item.get("id") == "anyrouter-retry"), None)
        if not plugin:
            raise RuntimeError("未安装 anyrouter-retry；启停助手不会修改供应商或安装插件")
        fields = (plugin.get("metadata") or {}).get("config_fields", [])
        supported = any(field.get("name") == "accept_new_requests" for field in fields)
        # An unloaded plugin may have no runtime metadata. Its selected binary
        # filename still tells this local helper whether it supports the switch.
        selected = re.search(r"-v(\d+)\.(\d+)\.(\d+)\.so$", plugin.get("path", ""))
        if not supported and selected and not plugin.get("registered"):
            supported = tuple(map(int, selected.groups())) >= (0, 1, 4)
        if not supported:
            raise RuntimeError("当前插件版本没有无损启停开关；需要 0.1.4 或更新版本")
        if args.action != "status":
            call("/anyrouter-retry/config", "PATCH", {"accept_new_requests": args.action == "on"})
            if args.action == "on" and not plugin.get("effective_enabled", plugin.get("enabled")):
                if isinstance(listing, dict) and listing.get("plugins_enabled") is False:
                    raise RuntimeError("CPA 插件系统总开关已关闭；助手不会自动开启其他插件")
                call("/anyrouter-retry/enabled", "PATCH", {"enabled": True})
        if not plugin.get("effective_enabled", plugin.get("enabled")) and args.action != "on":
            print(json.dumps({"host_enabled": False, "accept_new_requests": False, "note": "宿主已卸载此插件；无损关闭请使用 off 或 accept_new_requests=false"}, ensure_ascii=False))
            return
        deadline = time.monotonic() + 10
        while True:
            try:
                state = call("/anyrouter-retry/runtime", runtime=True)
                if args.action == "status" or state.get("accept_new_requests") == (args.action == "on"):
                    break
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
            if time.monotonic() >= deadline:
                raise RuntimeError("尚未确认运行时状态；请执行 status 核对")
            time.sleep(0.1)
        state["host_enabled"] = True
        state["note"] = "无损开关只影响后续请求；已接管请求继续。active_requests_current_instance 不包含热更新前的旧版本实例，不能用于证明物理卸载安全。"
        print(json.dumps(state, ensure_ascii=False, indent=2))
    except urllib.error.HTTPError as error:
        raise SystemExit("管理请求失败，HTTP " + str(error.code)) from None
    except (RuntimeError, urllib.error.URLError) as error:
        raise SystemExit(str(error)) from None
    except (OSError, http.client.HTTPException):
        raise SystemExit("CPA 管理连接未完成；请核对服务状态后执行 status") from None


if __name__ == "__main__":
    main()
