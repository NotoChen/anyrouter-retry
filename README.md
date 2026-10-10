# AnyRouter Retry 插件

面向 CPA v8 的 AnyRouter 原生执行器插件，当前版本 **0.1.12**，已按 CPA **v8.0.22 / dc339823e616d3abfff76c5abed0fd04d1b65001** 的 C ABI 和 host model callback 接口实现。

目标：AnyRouter 在生成内容前返回容量不足、限流、暂时性网关错误时，保留同一次 Codex SSE 连接，在插件内部立即重试。客户端收到保活事件，直到当前尝试开始生成内容；失败尝试的响应 ID、握手和错误不发给客户端。

## 分发方式

这个仓库发布的是 CPA 原生动态插件。CPA 直接加载对应平台的动态库并调用 v8 C ABI；插件通过 `model.route` 声明匹配范围，再通过 `executor.execute_stream` 调用 CPA 的 `host.model.execute_stream`。它不启动第二个代理服务，也不需要 CPAMA 才能参与请求。

每个 GitHub Release 按 CPA 插件商店约定提供平台归档和 `checksums.txt`：

```text
anyrouter-retry_0.1.12_linux_arm64.zip
checksums.txt
```

归档内是带版本号的动态库，例如 `anyrouter-retry-v0.1.12.so`。安装到 CPA 时，目标目录按 CPA 的 `plugins/<GOOS>/<GOARCH>` 组织；当前 Linux ARM64 容器对应：

```text
/CLIProxyAPI/plugins/linux/arm64/anyrouter-retry-v0.1.12.so
```

安装步骤：

1. 从 [Releases](https://github.com/NotoChen/anyrouter-retry/releases) 下载与 CPA 运行架构匹配的归档。
2. 校验 `checksums.txt`，解压动态库到 CPA 插件目录。
3. 在 CPA `config.yaml` 的 `plugins.configs` 中加入 [`config.example.yaml`](config.example.yaml) 的配置片段，并保持 `plugins.enabled: true` 与 `anyrouter-retry.enabled: true`。
4. 对被插件接管的 AnyRouter 凭据设置 `request-retry: 0`，避免 CPA 原生重试和插件重试叠加。
5. 通过 CPA 的配置重载机制加载新版本；更新时使用新的版本文件名，不要覆盖正在被进程使用的 `.so`。

仓库内的 `install.py` 是针对作者当前 Docker 部署的保守安装助手，会检查四个现有 AnyRouter 条目并原位备份/写入本机配置。它不是通用远程安装器；其他用户应按上面的下载、校验、复制和配置步骤操作，不能直接运行这个脚本修改自己的配置。

CPAMA 只提供管理界面：它可以承载插件注册的 dashboard iframe、复用管理登录并显示请求追踪；没有 CPAMA 时，CPA 仍然可以正常加载插件和执行重试。仅把仓库公开到 GitHub 也不会自动出现在 CPA 的“插件商店”：CPA 的 GitHub Release 安装需要插件注册中心提供对应的插件清单、版本、平台归档和校验文件。当前仓库已提供符合该约定的 Release 资产；若要进入公共商店，还需要向 CPA 使用的 registry 提交清单。

## 当前安装和配置

源码仓库：`https://github.com/NotoChen/anyrouter-retry`。当前版本的本机部署目录仍可能是 `~/cpa-manager-plus/plugins/anyrouter-retry`。

Linux arm64 / Debian 12 兼容动态库：`dist/anyrouter-retry-v0.1.12.so`。

运行时位置：`cli-proxy-api:/CLIProxyAPI/plugins/linux/arm64/anyrouter-retry-v0.1.12.so`。

插件使用宿主的 `host.model.execute_stream` 发起每次尝试，复用现有 AnyRouter API key、代理、原生 Codex 请求处理和调用记录。不额外保存一份密钥，也不手工维护会话 ID。读取宿主配置只是为了匹配模型、排除项和稳定的凭据 ID；密钥只参与 ID 的哈希计算，随后从凭据结构清除。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    anyrouter-retry:
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
```

四个当前可用的 AnyRouter 凭据各自设置 `request-retry: 0`，避免宿主重试与插件重试叠加。排除了所有模型的 `AnyRouter-zb` 不参与。

**本插件接管的请求不使用原生的重试次数决定是否继续，也不要求打开 `codex.stream-bootstrap-buffering`。** 已在全局 `request-retry: 0`、`stream-bootstrap-buffering: false` 的隔离 CPA 中验证了 111 次上游尝试在同一次客户端连接内最终成功。原生 buffering 打开时也经过验证。

实际部署的全局 `request-retry: 100`、`stream-bootstrap-buffering: true` 仍保留；它们用于其他原生请求。对插件接管的四个 AnyRouter 凭据，原生重试次数明确覆盖为 0。

## 在 CPA Manager Plus 中查看会话请求（0.1.5）

现有管理界面左侧新增 **AnyRouter 会话请求**，地址：

`http://127.0.0.1:19999/management.html#/plugin-pages/anyrouter-retry/0`

页面复用 CPAMP 已保存的登录状态，不要求另输管理密钥。它按 sessionId 分组，每个模型请求独立显示；展开后可看 threadId、父线程、turnId、代理名称、当前状态、总尝试/重试次数、首次输出耗时、最终凭据，以及逐次 AnyRouter 名称、顺序、HTTP 状态、错误 code、上游 trace 和耗时。支持会话搜索、状态筛选、分页和每 5 秒自动刷新。顶部统计针对当前页。

`Session-Id`、`Thread-Id` 和小型 `X-Codex-Turn-Metadata` 头提供会话身份，不读取对话正文。没有 sessionId 时明确显示未知；threadId 回退不能证明根会话身份。每次模型请求生成独立记录 ID，客户端 requestId 可能在同一线程重复，不能用于合并请求。

HTTP 200 可能仍携带 SSE 限流/失败。只有收到真实 `response.completed` 且提交到宿主下游流，才标记本次模型请求完成；不代表整个 Codex 任务结束，也不等同客户端已处理该事件。

默认记录到已有持久化插件卷，摘要与逐次日志分文件保存；默认保留 7 天，单次请求详细日志最多 32MiB。达到明细上限后计数继续，页面明确提示后续顺序未保存。磁盘写入失败只影响记录，不改变重试或输出，并在运行状态/页面提示。宿主重启后没有终态的旧记录显示“重启后结果未知”。清理在新请求触发，至多每小时一次，不是独立定时服务。

记录从 0.1.5 接管的新模型请求开始，不回填旧日志。热更新时已经交给旧版的请求继续由旧版处理；同一旧会话的下一次模型请求进入新版后即可看到。

实现使用 CPA 原生插件菜单和 iframe。静态 HTML 资源公开，但 runtime、requests、trace 数据接口均要求 CPA 管理认证，不增加公共数据接口。登录复用兼容当前 CPAMP 的 `cli-proxy-auth` / `enc::v1::`、`enc::v2::` 存储格式，仅在同源插件页面且 apiBase 匹配时启用；解码后的管理密钥只保留在页面内存。如果未保存登录凭据或未来 CPAMP 更改格式，此兼容方式可能需要调整。独立打开静态页面仍要求认证。

```sh
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/control.py dashboard
```

该命令打开 CPAMP 的菜单地址，浏览器验证使用连接到 Dia 的 Chrome DevTools MCP。

## 会话名称与列表优化（0.1.6）

会话组优先显示项目名和 Codex 保存的会话标题；每条请求直接展示当前/完成 AnyRouter、开始时间、总尝试/重试、耗时和最近失败原因。有实际子代理名称时单独展示，主线程不再重复“未提供代理名”。默认处理中优先，每组先展示最近 5 条（活动请求超过 5 条时全部展示活动请求）；支持展开更多和折叠会话。完整请求/sessionId 保留在可展开的详情中，支持复制。搜索同时覆盖标题和项目。

标题不是 Responses 协议提供的数据。`sync_session_labels.py` 只读本机 `.codex/state_*.sqlite` 的已记录 session/thread 对应的标题、项目目录末级名称和代理昵称；不读或修改对话正文。它写入现有插件持久卷内的 `session-labels.json`，管理接口读取该索引来补充显示，不影响路由和重试，索引缺失/损坏则使用短 ID。标题优先使用 Codex 手工命名的 name，否则使用 title，最多同步 160 个字符。仅按精确 ID 匹配，不猜测会话名称。

当前机器已启用 macOS LaunchAgent `local.cpa-anyrouter-session-labels`，每 60 秒同步一次，因此新会话/更名最多约一分钟后显示。已在 UI 中加载的标题在下次 5 秒刷新时更新。标题同步依赖本机 Docker、CPAMP 的管理密钥环境变量以及本地 Codex SQLite schema；其他客户端/机器不能据此取得本机会话标题。

```sh
# 手动同步一次
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/sync_session_labels.py
# 安装/移除本机自动同步（不启停重试插件）
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/sync_session_labels.py --install-auto-sync
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/sync_session_labels.py --remove-auto-sync
```

LaunchAgent 位于 `~/Library/LaunchAgents/local.cpa-anyrouter-session-labels.plist`。同步日志在插件目录 `logs/`，只输出数量或错误类型，不输出密钥和会话内容。分发时可选安装自动同步；脚本针对当前本机容器名、端口和默认 trace_dir，其他部署需调整。

## 会话组默认收起（0.1.7）

左侧会话组首次进入默认收起，仅显示会话名称、项目和当前页状态概况。点击会话标题或 +/− 展开/收起；“全部展开”展开当前页的所有会话，“全部收起”清除展开状态。浏览器保存手工展开的会话，自动刷新和重新打开页面保留选择，新会话默认收起。“只看此会话”保留为独立筛选按钮。

## 收起状态与流式处理（0.1.8）

会话收起时仍显示当前请求的状态、凭据、尝试/重试次数、耗时和最近失败。优先展示活动主线程请求，其次活动子线程，否则显示最新已记录请求；当前页的多线程处理中总数另行保留。展开状态不影响刷新。

每次重试都通过 CPA 的 host.model.execute_stream 发起，由 CPA auth manager 固定选中凭据、原生 Codex executor 发 HTTP 请求。宿主跳过发起插件自身，避免递归，不经过另一个 localhost HTTP 回环。当前凭据轮换由插件决定，首选凭据不遵从原生 session-affinity；不能将这种执行器接管称作仅观察原生失败的钩子。插件使用原始请求体，不重写提示词、reasoning 或模型参数。

流式处理只暂存 response.created/in_progress/queued/metadata 等启动事件；首个非启动内容事件到达即转发启动事件和当前内容，随后逐帧转发，不等待完整答案。错误返回前保留启动 ID 是为了避免把失败尝试的 response ID 暴露给 Codex，随后换一套 ID 继续同一连接。已输出内容的请求中断不会重放，以避免重复文本/工具调用。

原来的 max_bootstrap_bytes 同时限制单帧和首个内容事件，过于严格；0.1.8 拆成握手缓冲 8MiB 和单个 SSE 事件 64MiB，首个内容单独转发，整段回答无累计大小限制。64MiB 为单帧内存保护，当前 CPA 自己的上游单行 Scanner 限制为 50MiB；插件的新默认值允许这些单行和 SSE 字段封装。逐次日志的 32MiB 上限只限制记录，不限制输出。

当前 CPA 原生 stream-bootstrap-buffering=true 也作用于内部模型请求，因此 host.model.execute_stream 的等待可能包含上游响应、认证调度和 CPA 的启动事件缓冲。插件没有拿到的事件无法提前转发。retry_delay=0 表示错误返回后不额外等待，不表示可以在一个尚未返回的上游请求中提前重试。

0.1.8 新请求每次尝试记录 host_open_ms、first_host_frame_at、first_forwarded_at、parse_micros、emit_micros。详情显示 CPA 打开流的耗时、首个宿主数据帧到首次转发间隔、JSON 检测与下游 emit 累计耗时。首帧到转发间隔可包含等待后续内容；emit 包含客户端消费/回压，不应称作插件纯 CPU 耗时。计时只在既有生命周期保存点落盘，不逐 token 写磁盘。旧记录不补计时。

表格的 CPA 状态表示宿主接口返回状态；CPA 可把 HTTP 200 流内限流映射为 503，不能用它代替原始上游 HTTP 抓包。

## 插件自己的参数

宿主注册元数据已声明以下配置字段。CPA 插件配置页可读取它们；界面是否把 array 类型展示成列表取决于管理界面的渲染支持。

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `enabled` | `true`（插件内部）；宿主必须显式启用 | 是否接管匹配的请求 |
| `accept_new_requests` | `true` | 无损启停：false 时新请求走原生 CPA，已接管请求继续；宿主 enabled 保持 true |
| `config_path` | `/CLIProxyAPI/config.yaml` | 读取正在使用的 CPA 配置，自动发现凭据和模型 |
| `upstream_hosts` | `[anyrouter.top]` | 精确匹配上游域名，避免匹配其他供应商 |
| `models` | `[gpt-6-astra]` | 接管的客户端模型名，可识别模型后的推理参数括号 |
| `max_attempts` | `0` | 一次客户端请求内的总尝试次数；0 不设次数上限，正整数限制总尝试次数 |
| `retry_delay` | `0s` | 两次尝试之间额外等待时间；0s 表示错误返回后立即再请求 |
| `heartbeat_interval` | `5s` | 向客户端发 `data: {"type":"keepalive"}` 的间隔，最小 100ms |
| `max_bootstrap_bytes` | `8388608` | 仅暂存启动握手事件的内存上限，不限制首个内容事件或整个回答，超过时明确报错 |
| `trace_dir` | `/CLIProxyAPI/plugins/anyrouter-retry-data` | 会话、请求摘要和逐次尝试记录的持久化目录；当前使用已有插件持久卷 |
| `trace_retention_days` | `7` | 已结束记录保留天数，最小 1；当前宿主进程内的活动请求保留 |
| `trace_max_bytes_per_request` | `33554432` | 每条请求详细尝试日志的上限，最小 4096；达到上限后累计计数继续，页面提示明细被截断 |
| `max_sse_frame_bytes` | `67108864` | 单个完整/未完成 SSE 事件的内存上限；不限制整段回答总量，最小 1024 |
| `transient_codes` | 见下文 | 可重试错误 code/type 的识别项；自定义数组替换默认数组 |

默认 `transient_codes`：

```yaml
transient_codes:
  - rate_limit_reached
  - rate_limit_exceeded
  - rate_limit_error
  - too_many_requests
  - no_capacity
  - get_channel_failed
  - server_error
  - overloaded_error
  - service_unavailable
```

HTTP `408`、`425`、`429` 和暂时性 `5xx` 也会重试，包括实际日志出现的 `502`、`503`、`524`。`501`/`505` 不是默认的 HTTP 状态重试项，但明确匹配暂时性错误 code 时仍按 code 处理。生成前的连接重置、意外 EOF、未收到 `response.completed` 的结束也支持重试。

`retry_delay: 0s` 指插件不加 sleep/backoff。每次上游请求自身的网络、排队和响应时间仍然存在；前一次返回后，才会发下一次请求。

## 行为和边界

1. 接到匹配的流式 Responses 请求后，立即向客户端发送安全的 keepalive 数据事件。
2. 按现有配置解析所有可用 AnyRouter 凭据，遵守模型列表、模型排除项和显式禁用。
3. 顺序尝试这些凭据，每次尝试锁定一个凭据，由宿主执行实际请求。清除选中凭据的路由冷却状态，支持立即重试。
4. 暂存 `response.created`、`response.in_progress` 等握手。容量不足、限流和暂时性网关错误在内部消化，重新发起下一次尝试。
5. 开始产生正文、reasoning 输出或工具 item 时，发出该次尝试的握手与内容，并继续流式转发。只有收到真实 `response.completed` 才视为成功。
6. 客户端断开/取消后结束重试，并关闭已打开的宿主模型流。

请求内容错误，例如 `thinking_signature_invalid`、上下文超过上限、无效参数，不会靠重试修复，直接返回。凭据身份验证/模型访问失败会在当前请求内排除该凭据，尝试其他可用凭据；全部不可用时结束。

**已经发出正文、reasoning item 或工具调用后，不会自动从头重新生成。** 这时上游中断仍会传给客户端，否则有重复文本、重复工具调用的风险。这里缓冲的是生成前的启动事件，不是把完整答案等到结束才发送。

当前路由范围是流式 Codex/Responses。普通 Chat Completions、非流式请求、`/responses/compact` 仍走宿主原有路径。模型路由在供应商选择前执行，因此本版只接管独占 AnyRouter 的模型；如果未来给其他 Codex 供应商同时开放相同模型，会让该模型回到原生路由，避免插件把其他供应商的请求强制改走 AnyRouter。

上游失败仍会进入 CPA 的调用日志/用量统计。插件减少的是客户端看到的失败与重连，不会把真实的上游失败记录改成成功，也不能使容量不足的上游立即恢复。

## 0.1.3 成功响应解析修复

0.1.2 在真实 AnyRouter 链路中漏读了成功响应。AnyRouter 的 SSE 包含 `event:` 和 `data:` 两行；CPA 内部逐行交给插件时去掉了换行。旧解析器把它们直接拼成 `event: ...data: {...}`，因而未识别 `response.completed`，把已成功的请求当成流提前结束并继续重试。之前仅使用 `data:` 的模拟上游未覆盖该格式。

0.1.3 在 CPA 内部 chunk 之间恢复 SSE 字段边界，同时支持原有仅 data 的事件和网络拆分。模拟上游现在默认发送真实的 event/data 格式，完整集成测试通过。真实对照测试中，旧插件四次尝试有三次上游完成但客户端未收到；修正后，隔离 CPA 约 5.7 秒、正式 CPA 约 8.4 秒均返回 `AB_PROBE_OK` 和 `response.completed`。实际 Codex CLI 关闭客户端重试后约 18.3 秒完成 `LIVE_CLI_OK`，没有 reconnect。

详见 `VERIFICATION.md` 和 `integration/live-verification.json`。0.1.2 时代的保活记录只能证明连接仍开着，不能证明成功响应正确交付。

## 热更新修复

0.1.0/0.1.1 把 `plugin.quiesce` 当作 shutdown，更新版本时取消正在等待的旧请求。这会让客户端在只收到 keepalive 后遇到：

```text
upstream stream closed before a terminal event (last event: keepalive)
```

0.1.2 已分开处理：quiesce 保留现有请求，宿主把新请求交给新版本；真正 shutdown 才取消剩余请求。真实 CPA 热更新测试已验证旧连接继续重试并成功完成。服务停机、用户主动禁用/卸载插件和客户端取消仍会结束连接。

## 构建与验证

```sh
cd ~/cpa-manager-plus/plugins/anyrouter-retry
./build.sh
```

构建使用 `golang:1.26-bookworm`，包含 Go race 测试，与当前运行容器的 Debian 12 libc 匹配。本地 `go test -race ./...` 也可执行逻辑测试。

集成测试使用当前 CPA 镜像、虚拟凭据和本地 mock upstream，不消耗实际供应商额度。

```sh
python3 integration/mock_upstream.py
```

另一个终端启动隔离容器：

```sh
docker run -d --name cpa-anyrouter-retry-test \
  -p 127.0.0.1:19998:9999 \
  -v "$PWD/integration/config.yaml:/CLIProxyAPI/config.yaml:ro" \
  -v "$PWD/dist:/test-plugins/linux/arm64:ro" \
  local/cli-proxy-api:latest
python3 integration/check.py
```

等待隔离容器启动完成后再测试。检查项目包含连续 110 次 SSE 限流后第 111 次成功、HTTP 429/500/502/503/504/524、永久请求错误、输出后中断、延迟与拆分传输的 SSE。

`integration/check_trace.py` 验证真实 CPA 宿主传递的会话/线程头、凭据顺序、HTTP 状态、并发线程隔离和数据接口认证。

`integration/check_lifecycle.py` 验证等待期间停用、输出期间停用、开启时保留原生请求，以及连续切换 10 次。`integration/check_hot_reload.py` 会把 0.1.12 动态库复制为临时选择版本 0.1.13 的夹具，验证更新时旧连接继续重试并完成；它不代表发布了 0.1.13。请只对隔离容器运行，结束并停止容器后删除 dist 中的 0.1.13 夹具。

另用当前 **codex-cli 0.161.0** 做过实际客户端验证：客户端 `stream_max_retries=0`，空闲超时 600ms；插件每 250ms 保活，上游每次延迟 1.3s、连续失败两次。Codex 输出 `RETRY_OK` 并正常结束，没有 reconnect。

## 安装、停用与备份

`install.py` 是当前这台机器的安装助手，严格检查原配置的四个 AnyRouter 条目，并保持其余配置文本。已有插件配置时它会拒绝重复插入。更换本地插件版本应复制新的带版本号 `.so`，再触发一次配置重载；不要覆盖正在 mmap 使用的动态库文件。

安装前的完整配置备份位于 `backups/config-before-20261009-140802.yaml`，目录权限 700、文件权限 600。**备份包含原有密钥，分发插件时不要包含 backups。** 本地的 dist、备份和实际调用验证记录已列入 `.gitignore`。

## 无损启停（0.1.4）

保持 CPA 宿主的 `plugins.enabled` 和 `plugins.configs.anyrouter-retry.enabled` 为 true，通过插件配置页的 `accept_new_requests` 字段控制是否接收新请求。该字段已注册为 boolean，实际部署显式写入 true。

```sh
# 查看运行状态
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/control.py status
# 无损停用：当前请求继续，新请求走原生 CPA
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/control.py off
# 无损开启：后续请求重新进入插件
python3 ~/cpa-manager-plus/plugins/anyrouter-retry/control.py on
```

助手通过 CPA 管理 API 仅修改 `accept_new_requests`，保存配置后核对运行时是否生效。管理密钥从环境变量 `CPA_MANAGEMENT_KEY` 或本机 CPAMP 容器读取，不输出密钥。不重启服务、不卸载插件、不修改供应商和会话历史。

启停按每次模型请求生效。已经交给插件的请求使用原配置继续重试、转发并完成；同一个 Codex 会话后续发出的模型请求，包括执行工具后的下一次请求，按新的开关状态选择路径。停用期间原生请求的重试和失败表现由当时 CPA 原生配置决定。

**不要将 CPA 原生“禁用/卸载”按钮当成上述无损开关。** 当前 v8.0.22 的硬禁用会关闭插件回调和模型流，实际测试导致客户端流不完整，可能等到客户端空闲超时。插件无法在宿主已销毁回调后保留原连接。彻底卸载只能安排在已确认没有使用该插件的请求时进行；状态中的 active_requests_current_instance 仅计当前实例，不能据此断言热更新前旧实例已经排空。

0.1.4 的 shutdown 已等待后台请求线程和保活线程退出，再允许宿主释放 C 回调表，修复异步线程可能访问已释放宿主接口的隐患。真正 shutdown 仍会取消请求，因此需要保留进行中的请求时使用无损开关。

需要完全恢复安装前配置时，先比对之后新增的配置，再使用 backups 中备份；其中包含密钥，禁止随插件分发。写入单文件 Docker bind mount 的配置必须保持原 inode，避免 rename 后容器仍读取旧文件。

当前审查、证据和宿主限制见 [AUDIT.md](AUDIT.md)。

源码参考：CPA 仓库 `examples/plugin/executor/go/main.go` 的 C ABI，以及 `internal/pluginhost/host_model_stream_callbacks.go`、`sdk/pluginapi/types.go` 的宿主模型流接口。

## 主题兼容（0.1.9）

插件页面复用 CPA Manager Plus 已有的主题桥接：宿主给同源 iframe 注入 data-theme、data-cpamp-plugin-host 和颜色变量，页面使用这些变量绘制背景、面板、控件和文字，并为状态标签提供暗色配色。亮色、黑夜与自动模式跟随 CPAMP 实时切换，无需增加 CPA 配置。独立访问且没有宿主主题桥接时，页面使用系统 prefers-color-scheme。

## 会话工作台（0.1.12）

使用现有原生 HTML/CSS/JS 重构插件页面。顶部展示当前页会话、处理中、完成、经重试完成、失败/中断及尝试计数；左侧按会话显示项目/标题、当前供应商和状态，直接点击摘要查看当前请求，展开后选择历史请求。右侧优先展示结果、次数/耗时、供应商尝试顺序、逐次结果和供应商汇总，计时与标识默认收起。所有原有标识、Trace、失败码和明细分页均保留。

桌面窗口的列表与详情各自滚动；窄窗口上下排列。刷新保留同一请求的展开状态、滚动与控件焦点，切换请求显示加载骨架。主题继续由 CPAMP 同步，无外部字体或 UI 库；数字使用等宽布局，支持键盘焦点和减少动态效果偏好。此次只改管理页面及版本元数据，未改重试执行逻辑。
