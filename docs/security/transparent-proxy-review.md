# 透明代理安全检查与修复

检查日期：2026-11-20。审查基线：[0b36927](https://github.com/XavierJu007/Babel-Compiler/commit/0b369271da28b0e4fb083550e514571f978151c7)。

本轮确认并修复了本地管理接口、凭据转发、HTTP 重定向、领域资料的消息权限、候选缓存隔离和响应内存边界问题。默认透明代理同时收紧为**只压缩 `user` 文本**。

这不构成“模型输出绝不被投毒”的证明。程序可以确定地保持消息角色、拒绝跨源管理请求、禁止借用密钥和重定向；模型是否完整保留了引用范围、条件、否定与授权关系，仍需要语义判断。语义审计通过只能描述本次检查结果。

## 检查范围与证据

沿原文、领域词典、编译、审计、缓存、最终转发的路径检查源码，并使用隔离的本地 HTTP 服务重现数据流。测试中的密钥、提供方、候选和审计回复都是夹具数据，没有调用真实模型网关，没有使用真实用户凭据。

覆盖了恶意网页请求本地服务、导入不可信词典、不同凭据共用编译服务、提供方返回重定向或异常响应等边界。浏览器相关复现针对服务器接受的 HTTP 请求条件；没有把它描述为对所有浏览器版本的完整攻击实测。预设候选和 `pass` 回复用于检查控制流，不能证明真实模型会被注入成功，也不能证明真实模型能抵抗注入。

## 已修复的问题

### 1. 本地管理接口缺少浏览器来源边界

基线管理接口不核对 Host、Origin 或请求媒体类型。跨站网页能够构造携带 JSON 的简单 `text/plain` POST，改变词典或配置；试连接口还可能把已保存的密钥用于请求临时指定的地址。任意 Host 访问日志的服务端路径也没有被限制。这是本轮优先级最高的确定性权限问题，实际网页能否发出请求还受浏览器网络策略影响。

现在实际服务入口统一执行以下检查：

- 仅允许明确的环回监听地址；`localhost` 固定绑定 `127.0.0.1`。
- Host 必须匹配本次绑定地址或 `localhost`，端口必须匹配实际监听端口。
- 拒绝跨 Origin、不透明 Origin、跨站及同站但不同源的浏览器请求。
- API 和代理的写请求要求 `Content-Type: application/json`；已知长度和分块请求正文都受 8 MiB 限制。
- 页面禁止被嵌入；API/代理响应设置独立 CSP、禁止 MIME 嗅探与缓存，上游响应不能覆盖这些策略。流式 Flush 保持可用。

同源 Web UI 和不携带浏览器 Origin 的本机客户端仍可使用。**这不是本机进程身份认证**；本机进程仍在信任边界内。

实现与回归：[local_http_guard.go](../../babel-go/local_http_guard.go)、[local_http_guard_test.go](../../babel-go/local_http_guard_test.go)、[server.go](../../babel-go/server.go)。

### 2. 已保存的编译密钥可能被带到另一个目的地

基线代理在调用方没有提供认证信息时，会使用编译配置中的密钥；请求又可以指定最终上游地址。试连、临时编译参数及配置保存也存在“改地址但继承旧密钥”的路径。

现在两类凭据分开使用：

- 最终服务认证由代理调用方提供。OpenAI 使用 `Authorization`；Anthropic 使用 `x-api-key` 或 `Authorization`。缺少时返回 401，并且不会先触发编译或向上游发送请求。
- 若配置已有编译密钥，编译、校验、试连、Web 配置保存和 CLI `-save-config` 更改协议、主机或端口时，必须显式提供该地址的密钥。同源路径或查询参数调整仍可沿用配置密钥。
- 旧版配置迁移时，不把环境中的密钥隐式继承给另一个源。显式指定的配置密钥仍按用户选择使用。

这些检查不会验证调用方提供的密钥是否属于指定服务，也不会限制本机程序使用自己的密钥选择上游。

实现与回归：[credential_boundary.go](../../babel-go/credential_boundary.go)、[security_credentials_test.go](../../babel-go/security_credentials_test.go)、[security_transport_review_test.go](../../babel-go/security_transport_review_test.go)、[CLI 保存回归](../../internal/evalrun/cli_credentials_integration_test.go)。

### 3. HTTP 重定向会重放 prompt，部分认证头也会随行

基线 HTTP 客户端自动跟随重定向。隔离的 307 测试确认请求体会到达第二个主机；Anthropic 的 `x-api-key` 也可能被继续发送。只在 Babel 内部停止跟随仍不够，因为把原始 `Location` 返回给 SDK 可能让 SDK 继续跟随。

编译、审计、独立阅读、试连和最终代理现在共用拒绝重定向的客户端。最终代理把 3xx 转为明确的 502，不返回 `Location` 或提供方的重定向正文。需要配置最终请求地址。

实现与回归：[outbound_http.go](../../babel-go/outbound_http.go)、[outbound_http_test.go](../../babel-go/outbound_http_test.go)、[security_transport_review_test.go](../../babel-go/security_transport_review_test.go)。

### 4. 领域词典内容被放进编译模型的 system 消息

领域词条中的自然语言原来被拼入固定 system 提示词。用户导入或工作区覆盖的资料因此取得了不必要的消息权限。已确认的是错误的权限放置，不是任意模型上的注入成功率。

现在 system 只包含固定编译规则与内置示例；原文和选中的领域资料使用 `json.Marshal` 放在单独的 user 数据字段：

```json
{"original":"待编译原文","domain_guidance":"选中的领域参考资料"}
```

需要重组时，再加入 `candidate` 草稿字段。固定规则明确：原文是唯一语义来源，领域资料不能覆盖它、追加事实或发布编译器指令。模型返回的内容也只会作为原 user 文本块的字符串写回，不会解析成新的协议消息、角色或工具定义。

这降低了权限混淆，但 JSON 包装并不是模型注入的数学隔离。仍应检查自己导入的词典。

实现与回归：[dialects.go](../../babel-go/dialects.go)、[restructure.go](../../babel-go/restructure.go)、[security_prompt_test.go](../../babel-go/security_prompt_test.go)、[security_transport_review_test.go](../../babel-go/security_transport_review_test.go)。

### 5. 不同 API Key 共用了候选缓存与并发生成

相同地址、模型和原文可能因不同凭据而路由到不同租户。基线缓存与 singleflight 没有区分这些凭据，能够把甲凭据生成的候选复用给乙凭据。

现在候选身份包含凭据摘要和实际领域资料摘要，缓存版本升级为 `candidate-v3`；原始密钥不会写入缓存身份。相同凭据仍可复用生成。**缓存仅复用候选，每次代理采用之前仍重新审计**；本次修复没有把旧行为夸大为复用了审计通过结果。

实现与回归：[cache.go](../../babel-go/cache.go)、[security_prompt_test.go](../../babel-go/security_prompt_test.go)、[compiler_test.go](../../babel-go/compiler_test.go)。

### 6. 浏览器保存明文密钥，并可能自动恢复配置

停止把 API Key 写进 `localStorage` 备份，删除启动时根据浏览器备份自动恢复服务端配置的逻辑。页面启动时会清理当前 origin 中可识别的旧备份里的 `api_key`。

清理不能跨越浏览器同源边界：过去在其他主机别名或端口存储的备份不在本次页面的访问范围内。服务端配置仍需要保存编译密钥；配置与候选缓存文件加入 Git 忽略列表，减少误提交。这次不是密钥保险库改造，也没有声称发现了可利用的 UI XSS。

实现与回归：[index.html](../../babel-go/index.html)、[ui.test.cjs](../../babel-go/ui.test.cjs)。

### 7. 模型与非流式上游响应缺少内存边界

编译模型响应以前整体读入内存，最终代理的非流式响应也为了提取 usage 而整体读入。超时不能限制单位时间内收到的字节数。本轮确认的是资源边界缺失，没有进行破坏性的耗尽内存测试。

现在编译、审计与独立阅读的提供方响应信封最多 8 MiB；超限不解析、不采用、不写入候选缓存。最终服务响应边读边转发，只有非流式 usage 统计副本受 1 MiB 限制：超过时丢弃统计副本，**真实响应继续完整转发**。SSE 保持既有有界采样和透传行为。

实现与回归：[compiler.go](../../babel-go/compiler.go)、[proxy.go](../../babel-go/proxy.go)、[security_credentials_test.go](../../babel-go/security_credentials_test.go)、[proxy_response_limit_test.go](../../babel-go/proxy_response_limit_test.go)。

## 默认透明代理的内容边界

为了减少高权限内容被模型误改的机会，OpenAI 与 Anthropic 代理现在只压缩 `user` 的文本内容块。下列内容保留原值和结构：

- `system`、`developer`、Anthropic 顶层 `system`，包括块上的元数据。
- 工具名称、描述、参数 schema、调用及结果。
- assistant 历史、非文本内容块以及其他请求字段；数值继续保留原 JSON 数字表示。

这里的保持指字段内容与结构，不是整个请求体的字节顺序；代理会重新序列化 JSON。用户文本中的伪造 JSON 角色片段仍然只是字符串。

代价是这些受保护部分不再自动获得压缩收益。Web/CLI 的显式全文编译仍然可用，固定混合语言树表达也保持不变。这是默认权限边界的收紧，没有新增设置或模式。

## 回归验证

本轮在 Go 1.22.12 下构建实际服务二进制供集成测试使用；全包测试启用 `-race`，212 个顶层 Go 测试及其子用例通过。实际服务的 HTTP 编译、校验与报告集成，以及 CLI 保存配置测试均没有跳过。Node UI 测试 50 项通过。集成使用的服务二进制是普通构建，并未单独启用竞态检测。

可复现命令：

```bash
go build -o /tmp/babel-security ./babel-go
BABEL_TEST_BINARY=/tmp/babel-security go test -race ./... -count=1
node --test babel-go/ui.test.cjs
```

专项测试覆盖跨源词典覆盖、试连借用密钥、非法 Host 读取日志、不同凭据缓存/并发隔离、重定向第二跳零调用、JSON 角色片段不越界、高权限内容保持、超限响应回退与大响应完整转发。既有审计错误、旧缓存审计、数字与字面量、树结构、取消请求和 SSE 透传回归继续保留。

## 仍然成立的信任边界

1. **需要信任选定的编译、校验与最终服务。** 恶意提供方可能同时给出被改写的候选和虚假的通过结果；同一家模型反复自检不产生独立安全证明。本轮未测量真实模型的对抗通过率。
2. **原文回退不负责清洗原文。** 如果原始 prompt 本身包含恶意指令，透明转发会保留它。Babel 的目标是避免自己新增或改写权限，不是替消费模型授权执行操作。
3. **本机进程、工作目录和配置文件仍需可信。** 环回监听没有区分操作系统用户，磁盘候选缓存也不是防篡改存储。日志与候选可能含业务文本，不能把本机调试界面当公开服务使用。
4. **语义检查有盲区。** 引用变命令、条件方向或否定作用域改变，可能不表现为缺少某个字面量。确定性的格式检查与模型审计共同降低风险，无法宣布全部语义等价。

后续若继续投入，应优先把实际遇到的语义污染样本加入小型回归集，并在选定模型上评估；不需要为此扩展成多租户安全平台。
