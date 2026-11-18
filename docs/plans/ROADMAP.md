# Babel-Compiler 演进路线图 (Architecture & Engineering Roadmap)

> **核心定位**：面向 AI Agent 的零侵入高密度神经符号编译器与透明 LLM 代理网关（Go 单二进制交付）。实现「前端 UI 展示自然语言，底层通信流转高密度巴别语 DSL」。

---

## P0：真实 Token 计量与并发缓存闭环（高优先级）

当前本地 BPE 估算器已能感知 UTF-8 Byte-Fallback，但生产环境需要以**上游账单真实返回值**为准，并解决多 Agent 并发冷启动时的缓存击穿问题。

- [ ] **上游真实 `usage` Token 捕获**
    - **非流式请求**：解析上游响应体中的 `usage.prompt_tokens`，与本地估算的原始文本 Token 数对比，计算真实账单压缩率。
    - **SSE 流式请求**：转发前自动注入 `"stream_options": {"include_usage": true}`，在流式透传结束前截获最后一个包含 `usage` 的 chunk，异步写入代理透视日志。
    - **动态校准**：利用积累的真实 `prompt_tokens` 数据，微调本地 `estimateTokensBPE` 的权重参数。
- [ ] **并发请求合并（Singleflight 防击穿）**
    - 在 `doCompile` 核心层引入 `singleflight` 机制（仅使用 Go 标准库 `sync` + `chan` 实现，保持零外部依赖）。
    - 当多个 Sub-Agent 携带相同的 `System Prompt` 并发冷启动时，相同 `CacheKey` 仅向上游编译器发起一次请求，其余协程阻塞等待并共享结果。
- [ ] **持久化缓存 LRU 淘汰与单条重载**
    - 为 `babel.cache.json` 增加容量上限（默认 `2000` 条）与 `LastAccess` 时间戳，超出上限时按 LRU 策略淘汰冷数据。
    - 在 Web UI 右侧输出栏增加 **「强制重编译 (`no_cache: true`)」** 按钮，方便调试 Prompt 时刷新单条缓存。

---

## P1：Agent 协议深度适配（MCP Schema & Anthropic 协议）

现代 Agent（如挂载数十个 MCP Server 时）有 **60%～80% 的输入 Token 消耗在 `tools` 工具定义中**，仅压缩 `messages` 无法覆盖主要开销。

- [ ] **MCP / Function Calling `tools` Schema 压缩**
    - 拦截请求体中的 `tools[]` 数组，严格锁定 `function.name`、`type`、`required` 以及 `parameters.properties` 的字段键名（严禁修改，防止破坏工具调用解析）。
    - 仅提取并压缩 `function.description` 以及各参数的 `description` 字段（超过 40 字符时触发压缩），并将结果纳入同一套 SHA-256 确定性缓存池。
- [ ] **`response_format` 关键词熔断保护**
    - 检测请求体是否包含 `"response_format": {"type": "json_object"}`。
    - 若开启 JSON 模式，硬门禁（`checkDeterministicInvariants`）自动将 `json`（不区分大小写）加入强制保留词表，防止压缩后丢失 `json` 关键词导致上游接口直接返回 HTTP `400`。
- [ ] **兼容 Anthropic 原生协议 (`/v1/messages`)**
    - 新增 `/v1/messages` 代理端点，支持 `system`（字符串或内容块数组）与 `messages[].content` 的透明巴别语压缩，使 Claude Code 等原生 Anthropic 客户端无需中转即可直连使用。

---

## P2：方言词典热插拔（Multi-Dialect Hot-Reload）

将压缩规则从 `main.go` 硬编码中解耦，使其能够泛化到代码审计、SQL 生成、英文提示词精简等不同垂直领域。

- [ ] **外置方言模板目录**
    - 程序启动时自动扫描同级 `./dialects/*.md` 及 `~/.babel_dialects/*.md`（若目录不存在则自动释放内置的 `extreme.md`、`standard.md`、`token.md` 作为默认模板）。
- [ ] **动态路由与 CLI/Web 联动**
    - **透明代理**：支持通过请求头 `X-Babel-Dialect: <dialect_name>` 为不同业务线指定专属压缩词典。
    - **CLI 与 Web UI**：`babel -m <dialect_name>` 与前端下拉框自动列出所有已加载的方言文件，支持热重载。

---

## P3：生产级高可用与安全加固

确保透明代理在接入核心业务流时具备“故障绝对隔离”与“敏感数据零泄露”能力。

- [ ] **编译器熔断器（Circuit Breaker）**
    - 在透明代理模式下监控编译器模型的健康状态：若连续出现超时或非 `200` 错误达到 `3` 次，自动触发 `60s` 熔断窗口。
    - 熔断期间，所有经过 `/v1/chat/completions` 的请求 **0ms 直接透传原始自然语言**给上游 Agent，确保主业务可用性不受编译器波动影响；窗口期满后自动半开探测恢复。
- [ ] **代理透视日志敏感信息脱敏**
    - 增加 `redactSecrets(text string) string` 过滤器，在写入内存环形缓冲区 (`proxyLogs`) 前，自动使用正则遮蔽常见凭证（如 `sk-[a-zA-Z0-9]{20,}`、`Bearer <token>`、内网密码参数等），并在前端「透明代理透视」面板提供脱敏开关。
- [ ] **Web UI 关键不变量可视化校验胶囊**
    - 编译完成后，前端自动解析原文中的数字阈值（如 `169.254.169.254`、`8080`）与否定约束词（如 `不批量`、`不过期`、`只读不写`），在输出框底部渲染状态胶囊（`✓ 已保留` / `✕ 缺失`），实现肉眼秒级审计。