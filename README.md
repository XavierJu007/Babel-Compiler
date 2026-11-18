# Babel-Compiler

**把自然语言中的实体、关系与约束，编译成模型可读的紧凑表达。**

Babel 的思路是：把句子里展开叙述的关系，整理为语义图的紧凑文本表示，用公共术语和符号提高语义密度，争取以更少 Token 传递同样的要求。

两条原则：**不使用私有码本；输出无需额外图例即可理解。** 实体保留真实名称，符号使用公共含义。

## 能做什么

- 压缩长提示词、SOP、API 契约和工具描述中的重复措辞，保留条件、关系与约束。
- 通过 Web UI、命令行或 HTTP API 编译文本；支持自定义编译提示词模板。
- 作为 OpenAI Chat Completions / Anthropic Messages 代理，在转发请求前尝试压缩，编译报错时保留原文。

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

这是表达方式示意。实际输出与节省幅度取决于输入和模型；当前 Token 统计为估算，审计结果不代表语义无损保证。独立的领域词条库仍在设计中。

## 怎么用

需要 **Go 1.22+**，以及一个 OpenAI Chat Completions 兼容的编译模型服务。

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

在终端直接启动后，程序会从 `127.0.0.1:8080–8099` 选择可用端口并尝试打开浏览器。在「模型配置中心」填入 Base URL、API Key 和模型名，测试连接并保存，即可粘贴文本编译。

### 命令行

保存好模型配置后：

```bash
# 编译一段文本
./bin/babel -m token "你的提示词"

# 从文件读取，输出编译结果及审计报告
./bin/babel -f prompt.md -v

# 只输出编译文本，便于管道使用
./bin/babel -q -f prompt.md > compiled.txt

# 查看全部参数与可用模板
./bin/babel -h
./bin/babel -list-dialects
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
  -d '{"text":"你的提示词","mode":"token","verify":true}'
```

代理使用前，在模型配置中设置最终服务的 `upstream_url`。**配置中的模型用于编译，请求体中的 `model` 用于最终执行。**

客户端请求携带最终服务的认证信息：OpenAI 使用 `Authorization`，Anthropic 使用 `x-api-key`。

| 客户端协议 | Babel 代理端点 |
| --- | --- |
| OpenAI Chat Completions | `http://127.0.0.1:8080/v1/chat/completions` |
| Anthropic Messages | `http://127.0.0.1:8080/v1/messages` |

编译服务始终使用 OpenAI 兼容协议；最终上游需支持客户端所用协议。当前服务默认面向本机使用。

## 设计与后续改进

[语义图表达与领域词典设计](docs/design/semantic-compression.md) · [演进路线图](docs/plans/ROADMAP.md)

## License

[MIT](LICENSE)
