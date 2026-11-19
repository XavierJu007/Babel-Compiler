# Babel-Compiler

**把自然语言中的实体、关系与约束，编译成模型可读的紧凑表达。**

Babel 的思路是：把句子里展开叙述的关系，整理为语义图的紧凑文本表示，用公共术语和符号提高语义密度，争取以更少 Token 传递同样的要求。

两条原则：**不使用私有码本；输出无需额外图例即可理解。** 实体保留真实名称，符号使用公共含义。

## 能做什么

- 压缩长提示词、SOP、API 契约和工具描述中的重复措辞，保留条件、关系与约束。
- 独立组合领域表达库与输出预设；通过 Web UI、命令行或 HTTP API 编译文本。
- 按指定编码在本地计算文本 Token；可用只看候选的独立阅读器检查可读性，再与原文对照。
- 作为 OpenAI Chat Completions / Anthropic Messages 代理，检查失败或候选未减少所选计数时保留原文。

### 表达效果示意

原文：

> 对每条支付回调，首先验证签名，再检查签名是否覆盖 order_id 和 amount 字段。只有这两项检查都通过，并且订单当前状态为 pending 时，才允许将订单状态改成 paid。其他情况不得修改订单。

紧凑表达：

```text
每条支付回调:
先验签，再检查 {order_id, amount} ⊆ 签名覆盖字段。
仅当两项检查通过 ∧ 订单状态=pending:
  允许订单状态迁移: pending → paid
否则: 禁止修改订单
```

这是表达方式示意，实际输出与节省幅度取决于输入和模型。默认计数为估算，可选 `cl100k_base` / `o200k_base` 精确文本计数；审计通过不代表语义无损保证。

## 怎么用

需要 **Go 1.22+**，以及一个 OpenAI Chat Completions 兼容的编译模型服务。

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

在终端直接启动后，程序会从 `127.0.0.1:8080–8099` 选择可用端口并尝试打开浏览器。在「模型配置中心」保存 Base URL、API Key 和模型名，选择输出预设与领域，即可粘贴文本编译。内置公共符号、应用安全、API 契约和并发领域，可在「领域表达库」编辑词条。

### 命令行

保存好模型配置后：

```bash
# 编译一段文本
./bin/babel -m token "你的提示词"

# 组合领域，从文件读取并执行严格审计
./bin/babel -m standard -domains application-security,api-contract -gate -f prompt.md

# 指定目标编码，并用独立阅读检查候选（额外模型调用）
./bin/babel -token-encoding o200k_base -validation-mode blind -gate -f prompt.md

# 只输出编译文本，便于管道使用
./bin/babel -q -f prompt.md > compiled.txt

# 查看全部参数与可用模板
./bin/babel -h
./bin/babel -list-dialects
./bin/babel -list-domains
```

也可通过 `BABEL_API_KEY`、`BABEL_BASE_URL`、`BABEL_MODEL` 配置首次运行的编译服务；已有保存配置时，可在界面修改或使用 CLI 参数覆盖本次编译。

### HTTP API 与代理

```bash
./bin/babel -serve 127.0.0.1:8080
```

编译接口：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"你的提示词","mode":"token","domains":["api-contract"],"strict_gate":true}'
```

代理使用前，在模型配置中设置最终服务的 `upstream_url`。**配置中的模型用于编译，请求体中的 `model` 用于最终执行。**

客户端请求携带最终服务的认证信息：OpenAI 使用 `Authorization`，Anthropic 使用 `x-api-key`。
可用 `X-Babel-Dialect` 选择输出预设，`X-Babel-Domains` 以逗号分隔选择领域。

| 客户端协议 | Babel 代理端点 |
| --- | --- |
| OpenAI Chat Completions | `http://127.0.0.1:8080/v1/chat/completions` |
| Anthropic Messages | `http://127.0.0.1:8080/v1/messages` |

编译服务始终使用 OpenAI 兼容协议；最终上游需支持客户端所用协议。当前服务默认面向本机使用。

## 详细说明

[领域表达库](docs/usage/domain-library.md) · [Token 计数](docs/usage/token-counting.md) · [验证模式](docs/usage/validation.md) · [设计与路线图](docs/design/semantic-compression.md)

[评测入口](eval/README.md)：30 个待人工复核的中英文案例，默认离线检查；可记录指定模型的编译与验证结果。

## License

[MIT](LICENSE)
