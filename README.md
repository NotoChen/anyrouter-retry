# anyrouter-retry

`anyrouter-retry` 是面向 CPA v8 的原生动态插件，用于处理 AnyRouter 在流式 Responses 请求生成内容之前返回的限流、容量不足和暂时性网关错误。

插件在 CPA 内部重新发起上游尝试，保持同一个客户端 SSE 连接，并在收到可用上游的 `response.completed` 后才把成功结果交付给下游。这样，Codex 可以继续等待 CPA 的内部重试，减少因上游短暂失败触发的客户端 `reconnect`。

当前版本：**0.1.13**

仓库：[NotoChen/anyrouter-retry](https://github.com/NotoChen/anyrouter-retry)
许可证：[MIT](LICENSE)

## 功能概览

- 针对指定的 AnyRouter 域名和模型接管流式 Codex/Responses 请求。
- 通过 CPA 的 `host.model.execute_stream` 执行每次上游尝试，继续使用 CPA 的认证、代理和请求处理链路。
- 对 `429`、暂时性 `5xx`、`get_channel_failed`、容量不足、连接重置和提前结束的 SSE 流进行重试。
- `max_attempts: 0` 支持持续重试，`retry_delay: 0s` 支持在一次尝试结束后立即发起下一次尝试。
- 在生成内容之前暂存启动事件，并发送 SSE keepalive，使客户端连接保持打开。
- 只有收到真实的 `response.completed` 才将本次请求标记为完成。
- 生成内容、reasoning item 或工具调用已经转发后，不再从头重放，避免重复输出和重复执行工具。
- 将 sessionId、threadId、turnId、AnyRouter 尝试顺序、HTTP 状态、错误码和上游 Trace 持久化，供 CPAMA 管理页面查看。
- 支持 `accept_new_requests` 无损接收开关：停止接收新请求时，已经交给插件的请求继续运行。

## 工作方式

对于一个匹配的流式请求，插件按以下顺序工作：

1. 在 CPA 的模型路由阶段检查请求格式、模型名称和上游域名。只有匹配配置的独占 AnyRouter 模型才进入插件。
2. 从 CPA 当前配置中发现符合条件的凭据，并通过 CPA 的认证管理器获得可用凭据标识。插件不会复制或持久化 API key。
3. 在请求开始时建立候选顺序，并按凭据稳定 ID 轮询。每次准备下一次生成前重试时，插件只检查 CPA 配置文件的大小和修改时间；检测到保存后才重新解析。新凭据追加到当前请求队列末尾，配置重排不会改变已经走过的轮询位置。
4. 通过 `host.model.execute_stream` 发起一次上游请求，同时向客户端发送 keepalive。
5. 暂存 `response.created`、`response.in_progress` 等启动事件。若上游在产生内容前返回可重试错误，则关闭本次流并轮换到下一组凭据。
6. 如果配置发生变化，插件会在下一次重试前增量发现新增或更新的凭据。如果 CPA 返回当前 `auth_index` 已不存在，再执行完整刷新并移除已删除或已替换的旧凭据；这也覆盖 CPA AuthManager 异步刷新窗口。
7. 首个内容事件到达后，立即转发暂存的启动事件和当前内容，之后逐帧转发上游 SSE。
8. 收到 `response.completed` 后结束请求并记录成功；客户端取消、永久错误、凭据全部不可用或达到显式尝试次数上限时结束请求。

这里的“运行中的请求”是一个已经进入 CPA `executor.execute_stream` 的单次 HTTP/SSE 请求，包含插件为它执行的多次上游尝试；它通常对应 Codex 一个 turn。Codex 的 session 会连续产生多个 turn，每个 turn 都会创建新的 CPA 请求并读取当时的最新配置。新增供应商只能加入当前请求尚未提交输出的后续尝试，不能把已经输出过的请求从头重放。

插件不会改写请求体、提示词、reasoning 参数或模型参数，也不会修改 Codex 会话历史。

插件由 CPA 直接加载和调用，CPAMA 只提供可选的管理页面与状态展示；没有 CPAMA 时，CPA 仍可执行路由和重试。

## 兼容性

当前版本按 CPA **v8.0.22**（`dc339823e616d3abfff76c5abed0fd04d1b65001`）的插件 C ABI 和宿主模型流回调接口实现，并在 Linux arm64 / Debian 12 环境完成验证。其他 CPA 版本、操作系统或架构需要使用对应平台的动态库并重新验证宿主接口兼容性。

支持的请求范围：

- 流式 `codex` 和 `openai-response` 请求；
- 由 `models` 和 `upstream_hosts` 明确匹配的请求；
- CPA 能够通过 `host.model.execute_stream` 执行的上游调用。

普通 Chat Completions、非流式请求以及 `/responses/compact` 不由本插件接管，继续使用 CPA 原生路径。

## 安装

### 1. 下载动态库

从 [Releases](https://github.com/NotoChen/anyrouter-retry/releases) 下载与 CPA 运行平台匹配的文件，并校验 `checksums.txt`。

当前 `v0.1.13` Release 提供：

```text
anyrouter-retry-v0.1.13.so
anyrouter-retry_0.1.13_linux_arm64.zip
checksums.txt
```

### 2. 放置动态库

将动态库放入 CPA 的插件目录，目录格式为：

```text
<CPA 插件目录>/<GOOS>/<GOARCH>/anyrouter-retry-v0.1.13.so
```

例如 Linux arm64：

```text
/CLIProxyAPI/plugins/linux/arm64/anyrouter-retry-v0.1.13.so
```

版本升级时使用新的版本文件名，并按 CPA 支持的方式触发插件重载。不要覆盖仍被运行中进程加载的动态库文件。

### 3. 配置 CPA

将 [`config.example.yaml`](config.example.yaml) 中的插件配置合并到 CPA 的 `config.yaml`。最小示例：

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

插件系统总开关、插件自身的 `enabled` 和 `accept_new_requests` 都需要按预期设置。插件通过 `config_path` 读取 CPA 正在使用的配置，因此该路径必须指向 CPA 容器或进程内实际可读的文件。

### 4. 关闭匹配凭据的 CPA 原生重试

对所有会被本插件接管的 AnyRouter 凭据设置：

```yaml
request-retry: 0
```

插件只把 `request-retry: 0` 的匹配凭据加入候选池，以避免 CPA 原生重试和插件重试叠加，造成次数失控、等待时间放大或调用记录难以区分。供应商在可视化界面保存期间暂时缺少该字段，或明确保留原生重试的凭据，会被插件跳过；如果没有其他合规 AnyRouter 凭据，本次请求交回 CPA 原生路由。这样编辑一个供应商不会阻断其他已经配置正确的供应商。

### 5. 验证加载状态

通过 CPA 管理接口或 CPAMA 插件页面确认插件已经注册、版本正确、`accept_new_requests` 为 `true`。如果使用仓库内的控制脚本，可执行：

```sh
python3 control.py status
```

`install.py` 是针对对应的本地 Docker 部署结构的安装助手，会修改本机 CPA 配置并创建包含密钥的备份。它不适合作为通用安装器，分发时请使用 Release 文件和上面的手动安装流程。

## 配置参考

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 插件实例是否参与路由。需要保持启用，才能使用无损接收开关。 |
| `accept_new_requests` | `true` | 是否接收新的匹配请求。设为 `false` 后，后续请求回到 CPA 原生路由，已接管请求继续运行。 |
| `config_path` | `/CLIProxyAPI/config.yaml` | CPA 配置文件路径，用于发现符合条件的现有凭据。 |
| `upstream_hosts` | `[anyrouter.top]` | 精确匹配的上游主机名列表。 |
| `models` | `[gpt-6-astra]` | 插件接管的模型名称列表。模型名称后的推理参数括号会被忽略。 |
| `max_attempts` | `0` | 单个客户端请求的总尝试次数。`0` 表示不设置插件侧次数上限；正整数包含第一次请求。 |
| `heartbeat_interval` | `5s` | 发给客户端的 SSE keepalive 间隔，最小为 `100ms`。 |
| `retry_delay` | `0s` | 两次尝试之间的额外等待时间。`0s` 表示前一次尝试返回后立即开始下一次。 |
| `max_bootstrap_bytes` | `8388608` | 仅限制尚未转发的启动握手事件缓冲区，默认 8 MiB。 |
| `max_sse_frame_bytes` | `67108864` | 单个 SSE 事件的内存上限，默认 64 MiB；不限制完整回答的累计大小。 |
| `transient_codes` | 见下文 | 可重试错误 `code` 或 `type` 的匹配列表；配置后会替换默认列表。 |
| `trace_dir` | `/CLIProxyAPI/plugins/anyrouter-retry-data` | 请求摘要和逐次尝试记录的持久化目录。 |
| `trace_retention_days` | `7` | 已结束记录的保留天数，最小为 1。清理在新请求触发，最多每小时执行一次。 |
| `trace_max_bytes_per_request` | `33554432` | 单条请求的详细尝试日志上限，默认 32 MiB。达到上限后计数继续，但后续明细会被标记为截断。 |

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

数组配置会替换默认数组。自定义时应保留仍需要处理的默认错误码。

## 重试规则

### 默认会重试

- HTTP `408`、`425`、`429`；
- HTTP `5xx`，包括常见的 `500`、`502`、`503`、`504` 和 `524`；
- `transient_codes` 中匹配的错误 `code` 或 `type`；
- 连接重置、连接拒绝、TLS 握手超时、I/O 超时、意外 EOF；
- 上游在 `response.completed` 之前关闭流。

### 直接结束或切换凭据

以下请求问题无法通过重复发送同一请求修复，会直接返回：

- `thinking_signature_invalid`；
- `invalid_request_error`、`invalid_argument`、`invalid_json`；
- `context_length_exceeded`；
- `unsupported_parameter`。

凭据无效、模型不可用或权限错误（例如 `401`、`403`、`404`、`invalid_api_key`、`model_not_found`）会将当前凭据标记为不可用，并尝试其他符合条件的凭据。全部凭据都不可用时，请求结束。

`max_attempts: 0` 仍会在客户端取消、连接断开、永久错误、凭据耗尽或宿主关闭时结束。`retry_delay: 0s` 只移除插件额外增加的等待；一次上游请求自身的网络、排队和响应时间仍然存在，插件无法在上游请求尚未返回时提前开始下一次尝试。

### 已开始输出后的边界

插件以首个正文、reasoning 或工具 item 被转发为提交点。提交点之后的上游中断不会从头重放，因为重放可能导致重复文本、重复 reasoning 或重复工具调用。该场景可能仍由客户端显示中断或触发客户端侧重连。

## CPA 原生配置关系

本插件接管的请求在插件内部决定是否继续尝试；只有 `request-retry: 0` 的匹配凭据会进入插件候选池，以保持调用次数和等待行为可控。其他匹配凭据会被跳过，避免配置编辑窗口或某个供应商的原生重试设置阻断整个 AnyRouter 候选池。

`codex.stream-bootstrap-buffering` 不属于本插件的必需开关。插件分别管理自己的启动事件缓冲和单个 SSE 帧大小；该 CPA 配置保持启用或关闭都不会改变插件的重试判定。CPA 在 `host.model.execute_stream` 内部的认证调度、网络等待和启动事件处理仍会影响每次尝试返回给插件的时间。

客户端收到 keepalive 只表示连接仍在等待，不能作为上游成功依据。管理页面只有在插件收到并提交 `response.completed` 后才显示请求完成。

上游失败仍会进入 CPA 的调用日志和用量统计。插件减少的是客户端在生成前看到的失败与重复连接，不会把真实的上游失败记录改写成成功，也不能使已经耗尽容量的上游立即恢复。

## CPAMA 管理页面与请求追踪

插件注册了一个 CPA 管理资源，CPAMA 可通过插件菜单打开：

```text
/management.html#/plugin-pages/anyrouter-retry/0
```

页面要求 CPA 管理认证，并按 sessionId 分组展示：

- 会话标题、项目和代理名称（存在本地索引时）；
- 当前状态、开始时间、总尝试次数、重试次数和耗时；
- 最终使用的 AnyRouter 凭据；
- 每次尝试的顺序、凭据、CPA 返回状态、结果、错误码、上游 Trace 和耗时；
- 当前请求的 `sessionId`、`threadId`、`parentThreadId` 和 `turnId`。

页面支持搜索、状态筛选、分页、会话折叠/展开和自动刷新。桌面端显示列表与详情两栏，窄窗口自动切换为单栏；页面跟随 CPAMA 的亮色、暗色或自动主题。

记录说明：

- 只记录插件接管的新模型请求，不回填启用前的历史请求；
- `response.completed` 表示成功结果已提交给 CPA 下游流，不代表整个 Codex 任务已经完成；
- 记录目录写入失败只影响追踪明细，不改变重试和输出流程；
- 宿主重启前未得到终态的旧记录会显示为结果未知；
- 详细尝试日志达到 `trace_max_bytes_per_request` 后，汇总计数继续累加，后续顺序可能不再完整。

### 可选的会话标题同步

`sync_session_labels.py` 只读本机 Codex SQLite 状态库中与插件已记录 ID 精确匹配的标题、项目目录和代理昵称，将索引写入 `trace_dir`。它不读取或修改对话正文，也不参与路由和重试。

```sh
# 手动同步一次
python3 sync_session_labels.py

# macOS：每 60 秒自动同步
python3 sync_session_labels.py --install-auto-sync

# 移除自动同步
python3 sync_session_labels.py --remove-auto-sync
```

该脚本依赖本机 Docker、CPA 管理认证和 Codex SQLite schema，其他部署需要根据实际路径和容器名称调整。

## 无损启停与生命周期

推荐使用 `accept_new_requests` 控制插件接收状态：

```sh
python3 control.py status
python3 control.py off
python3 control.py on
```

- `off`：只影响后续匹配请求；正在插件内重试或输出的请求继续使用创建时的配置。
- `on`：后续匹配请求重新进入插件；已经走 CPA 原生路径的请求不会迁移。
- 配置重载：已经运行的请求继续使用创建时的插件配置；在尚未提交正文、reasoning 或工具调用的生成前重试阶段，检测到 CPA 配置保存后会把新增凭据安全追加到当前请求队列。配置删除或 API key 替换会在 CPA 明确报告旧凭据不存在后完成清理。新的请求从最新配置开始。
- 配置同步保护只在 CPA 报告凭据不存在或配置文件正处于保存窗口时等待最多约 500ms；普通的 `retry_delay: 0s` 上游重试仍然立即进行。
- 热更新：旧实例保留已有连接，新实例处理后续新连接。

CPA 的宿主硬禁用、删除插件或关闭全局插件系统会销毁插件回调。此类操作可能中断正在运行的客户端流，无法提供无损保证。需要保留进行中的请求时，应先使用 `accept_new_requests: false`，等待活动请求结束后再进行卸载或停机。

## 构建与测试

### 构建动态库

```sh
./build.sh
```

构建脚本在 `golang:1.26-bookworm` 容器中执行 race 测试，并生成：

```text
dist/anyrouter-retry-v0.1.13.so
```

也可以直接运行本地测试：

```sh
go test ./...
go test -race ./...
python3 -m py_compile control.py install.py sync_session_labels.py integration/*.py
```

### 集成验证

集成测试使用隔离的 CPA 容器和仓库内的 mock upstream，不需要真实供应商凭据。测试覆盖：

- 连续限流后成功；
- HTTP `408`、`425`、`429` 和暂时性 `5xx`；
- SSE 分片、延迟和真实 `event`/`data` 格式；
- 永久请求错误和 `thinking_signature_invalid`；
- 已输出后中断不重放；
- 并发请求的 session/thread 隔离；
- 管理接口认证、无损启停和热更新。

完整的验证边界和宿主限制见 [`VERIFICATION.md`](VERIFICATION.md) 与 [`AUDIT.md`](AUDIT.md)。

## 安全与数据边界

- API key 保留在 CPA 配置和认证管理器中，插件只使用凭据标识发起宿主调用。
- 凭据 ID 由 CPA 约定的字段哈希派生；插件发现凭据后会清除内存中的 key 字段，不将密钥写入 trace。
- Trace 只保留会话标识、请求元数据、状态、错误归类和上游 Trace，不保存提示词、对话正文或完整上游响应。
- 管理数据接口使用 CPA 管理认证；静态 dashboard 页面本身不提供未授权的 trace 数据接口。
- `trace_dir` 应放在受 CPA 进程保护的持久卷中，并限制目录访问权限。
- `install.py` 生成的配置备份可能包含原始密钥，必须限制权限并避免提交到公共仓库。

## 仓库结构

```text
anyrouter-retry/
├── *.go                         # CPA 插件、重试执行器和追踪实现
├── dashboard.html               # CPAMA 管理页面
├── config.example.yaml          # 配置示例
├── build.sh                     # 可复现构建入口
├── control.py                   # 无损启停与状态查询
├── sync_session_labels.py       # 可选的会话标题索引同步
├── integration/                 # 隔离 CPA 与 mock upstream 集成测试
├── AUDIT.md                     # 审查记录与宿主边界
├── VERIFICATION.md              # 可复现验证记录
└── LICENSE
```

## 发布说明

当前公开版本为 `v0.1.13`。发布动态库、平台归档和校验文件位于 [GitHub Releases](https://github.com/NotoChen/anyrouter-retry/releases)。将仓库公开到 GitHub 不会自动把插件加入 CPA 的公共插件注册中心；公共商店仍需要对应的插件清单、版本信息、平台归档和校验值。
