# 透明代理传输安全与修复记录

更新日期：2026-11-20。上一轮审查基线：[0b36927](https://github.com/XavierJu007/Babel-Compiler/commit/0b369271da28b0e4fb083550e514571f978151c7)；上一轮修复：[381efa0](https://github.com/XavierJu007/Babel-Compiler/commit/381efa0)。

当前安全目标是保护 Babel 与所选模型、最终服务之间的传输，防止网络中间人窃听或篡改 prompt、响应及凭据。上一轮的本地管理、凭据、重定向、缓存与资源边界修复继续保留；本轮补齐远程 HTTPS、TLS 验证与环回直连规则。

`381efa0` 把压缩范围限制为仅 `user`，这是偏离使用者需求的过度限制，现已纠正：恢复 system、user 与工具描述的压缩，只替换对应文本字段。等义校验继续用于防止压缩改变原意，失败则保留原文；它不审查使用者自己的 prompt 内容，也不承担消费模型的内容过滤。

## 检查范围与证据

上一轮沿原文、领域词典、编译、审计、缓存、最终转发的路径检查源码，并使用隔离的本地 HTTP 服务重现数据流。测试中的密钥、提供方、候选和审计回复都是夹具数据，没有调用真实模型网关，没有使用真实用户凭据。本轮传输检查使用本地 HTTP、TLS 和代理夹具，分别观察连接是否被接受、是否发送业务请求以及目标 TLS 是否仍被验证。

覆盖了恶意网页请求本地服务、导入不可信词典、不同凭据共用编译服务、提供方返回重定向或异常响应等边界。浏览器相关复现针对服务器接受的 HTTP 请求条件；没有把它描述为对所有浏览器版本的完整攻击实测。预设候选和 `pass` 回复用于检查控制流，不能证明真实模型会被注入成功，也不能证明真实模型能抵抗注入。

## 本轮：网络中间人防护

此前客户端已经拒绝重定向，但仍接受任意远程 `http://` 地址；标准 HTTPS 证书校验不能保护主动选择明文连接的请求。本轮对编译、等义校验、独立阅读、试连及最终代理统一采用下列传输规则：

| 连接 | 当前行为 |
| --- | --- |
| 远程 HTTPS | 允许；最低 TLS 1.2，按系统信任根验证证书链、目标主机名和证书有效期 |
| 远程 HTTP，包括普通域名和局域网地址 | 发送前拒绝，不降级为明文 |
| 明确的环回 HTTP | 仅允许 URL 中写明 `127.0.0.0/8`、`::1` 或 `localhost` |
| `localhost` | 固定连接 `127.0.0.1`，不通过 DNS 选择其他目的地；环回请求不使用环境代理 |
| 远程 `HTTPS_PROXY` | 保留正常 CONNECT 隧道；隧道中的目标连接仍执行 TLS 校验 |
| HTTP 重定向 | 全部拒绝，包括 HTTPS 到 HTTP；最终代理返回 502，不转发 `Location` |

HTTP 的例外取决于 URL 主机本身是明确的环回地址；普通域名即使解析到环回，也不会因此获准使用明文。HTTPS 没有跳过证书验证的开关，不能通过关闭验证来让不受信任、过期或主机名不符的证书通过。

这些规则保护网络路径上的机密性与完整性，依赖操作系统的信任根和运行主机保持可信。如果已安装了能为目标主机签发证书的恶意受信根，标准验证会按这份信任接受证书；本工具不承诺抵御这种情况，也不承诺抵御被控制的本机或恶意提供方。本机 HTTP 例外位于本机进程的信任范围内。

**本轮验证结果：** Go 1.22.12 构建成功，全量 `go test -race ./... -count=1 -v` 中 224 个顶层测试及其子用例通过，无跳过；Node UI 测试 50/50 通过。通过 `BABEL_TEST_BINARY` 启用了实际 Babel 二进制，HTTP 服务的 2 项集成测试和 CLI 保存配置的 3 种场景均执行通过。服务二进制使用普通构建，竞态检测用于 Go 测试运行包。

传输专项使用本地受控 HTTP、TLS 与 CONNECT 夹具，结果如下；未调用真实网关或使用真实凭据：

| 验证场景 | 观察结果 |
| --- | --- |
| 远程明文 HTTP | 在发送前拒绝；不发起目标连接 |
| 未知 CA、主机名不符、证书过期或尚未生效、仅支持 TLS 1.1 | 连接被拒绝，业务 HTTP handler 调用数为 0 |
| 有效证书与 TLS 1.2 / 1.3 | 正常完成请求 |
| HTTPS 重定向到 HTTP | 不跟随，第二跳调用数为 0 |
| CONNECT 隧道 | 合法目标成功，伪造端点被 TLS 拒绝；代理捕获的数据不含夹具 prompt 和 Key 的明文 |
| 环回及编译传输失败 | 环回直连和环境代理规则通过；编译连接被拒绝时，最终代理保留原文继续转发 |

恢复后的 system、user 和工具描述压缩，以及消息角色、工具结构、非目标字段保持，也通过代理回归。测试验证具体传输与字段处理行为，不把模型预设回复当成真实模型语义准确率。

本轮可复现命令：

```bash
go build -o /tmp/babel-mitm ./babel-go
BABEL_TEST_BINARY=/tmp/babel-mitm go test -race ./... -count=1 -v
node --test babel-go/ui.test.cjs
```

实现与回归见 [outbound_http.go](../../babel-go/outbound_http.go)、[地址和代理规则](../../babel-go/outbound_policy_test.go)、[TLS 与 CONNECT 验证](../../babel-go/outbound_tls_test.go)、[代理压缩范围与目的地检查](../../babel-go/security_transport_review_test.go)。压缩范围见下文；其余历史发现与保留的修复如下。

## 上一轮已修复的问题

### 1. 本地管理接口缺少浏览器来源边界

基线管理接口不核对 Host、Origin 或请求媒体类型。跨站网页能够构造携带 JSON 的简单 `text/plain` POST，改变词典或配置；试连接口还可能把已保存的密钥用于请求临时指定的地址。任意 Host 访问日志的服务端路径也没有被限制。这是上一轮确认的权限问题，实际网页能否发出请求还受浏览器网络策略影响。

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

现在内部编译请求的 system 只包含固定编译规则与内置示例；原文和选中的领域资料使用 `json.Marshal` 放在单独的 user 数据字段：

```json
{"original":"待编译原文","domain_guidance":"选中的领域参考资料"}
```

需要重组时，再加入 `candidate` 草稿字段。固定规则明确：原文是唯一语义来源，领域资料不能覆盖它、追加事实或发布编译器指令。模型返回的内容只会作为字符串写回原文本字段，不会解析成新的协议消息、角色或工具定义；被代理请求的原 system 文本压缩后仍位于原 system 字段。

这是编译输入的结构边界，服务于原意保留，不是对使用者 prompt 的内容审查；JSON 包装也不证明模型一定正确理解。

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

编译模型响应以前整体读入内存，最终代理的非流式响应也为了提取 usage 而整体读入。超时不能限制单位时间内收到的字节数。上一轮确认的是资源边界缺失，没有进行破坏性的耗尽内存测试。

现在编译、审计与独立阅读的提供方响应信封最多 8 MiB；超限不解析、不采用、不写入候选缓存。最终服务响应边读边转发，只有非流式 usage 统计副本受 1 MiB 限制：超过时丢弃统计副本，**真实响应继续完整转发**。SSE 保持既有有界采样和透传行为。

实现与回归：[compiler.go](../../babel-go/compiler.go)、[proxy.go](../../babel-go/proxy.go)、[security_credentials_test.go](../../babel-go/security_credentials_test.go)、[proxy_response_limit_test.go](../../babel-go/proxy_response_limit_test.go)。

## 当前透明代理的压缩范围

`381efa0` 的仅 user 策略已经撤回。当前按既有用途压缩这些文本：

| 协议字段 | 压缩内容 |
| --- | --- |
| OpenAI `messages` | `system` 和 `user` 的字符串内容或显式 `text` 块 |
| Anthropic | 顶层 `system` 的字符串或 `text` 块，以及 `messages` 中的 user 文本 |
| OpenAI / Anthropic 工具 | 工具的 `description`，参数 schema 根节点及沿 `properties`、`items` 遍历到的 `description` |

每个候选只替换对应文本字段；通过等义校验且计数更短才采用，否则保留该字段的原文。`developer`、assistant 历史、工具调用及结果不压缩；工具名、参数名、类型、枚举、必填约束、块元数据与其他非文本字段保持原值和结构。生成结果中的 JSON 或角色声明仍是字符串，不能创建协议角色或替换工具结构。

这里的保持指字段内容与结构，不是整个请求体的字节顺序；代理会重新序列化 JSON，并继续保留数值的 JSON 数字表示。固定混合语言树表达、Web/CLI 全文编译及等义失败回退保持不变。

## 上一轮回归验证记录（381efa0）

上一轮在 Go 1.22.12 下构建实际服务二进制供集成测试使用；全包测试启用 `-race`，212 个顶层 Go 测试及其子用例通过。实际服务的 HTTP 编译、校验与报告集成，以及 CLI 保存配置测试均没有跳过。Node UI 测试 50 项通过。集成使用的服务二进制是普通构建，并未单独启用竞态检测。这是 `381efa0` 的历史验收记录，不能据此判断本轮 TLS 与压缩范围变更是否通过。

可复现命令：

```bash
go build -o /tmp/babel-security ./babel-go
BABEL_TEST_BINARY=/tmp/babel-security go test -race ./... -count=1
node --test babel-go/ui.test.cjs
```

当时专项测试覆盖跨源词典覆盖、试连借用密钥、非法 Host 读取日志、不同凭据缓存/并发隔离、重定向第二跳零调用、JSON 角色片段不越界、当时的仅 user 策略、超限响应回退与大响应完整转发。仅 user 策略已按当前范围纠正；其余既有审计错误、旧缓存审计、数字与字面量、树结构、取消请求和 SSE 透传回归继续保留。

## 仍然成立的信任边界

1. **系统信任根和运行主机需要可信。** TLS 按系统信任决定证书是否有效，无法排除已安装的受信恶意根证书；被控制的本机也可直接读取进程或配置中的数据。
2. **需要信任选定的编译、校验与最终服务。** TLS 验证连接目标，不能证明提供方诚实，或证明其返回的候选、审计结论正确。
3. **本机进程、工作目录和配置文件仍需可信。** 环回监听没有区分操作系统用户，磁盘候选缓存也不是防篡改存储。日志与候选可能含业务文本，不能把本机调试界面当公开服务使用。
4. **等义校验用于压缩质量。** 原文回退不清洗内容；模型可能误读条件、引用或否定范围，检查通过也不是形式上的语义等价证明。这与网络中间人防护是两个不同的验证目标。

维护继续围绕实际可复现的传输和压缩问题，不增加内容审查流程或多租户平台。
