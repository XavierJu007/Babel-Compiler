# Babel-Compiler

**把自然语言提示词编译成高密度巴别语的小工具。**

Babel 用公开术语、缩写、符号和紧凑层级直接表达主体、动作、对象、条件与约束，跨章节重组前置条件、执行依赖与异常，省掉完整句子及原编号提纲。输出是模型可读的图式文本，保留必要短语与技术字面量。Web、命令行、API 和代理固定使用同一套 `mixed` 规则；专业词典由你自己编写。

两条原则：**不使用私有码本；输出无需额外图例即可理解。** 自解释不要求写成完整自然语言句子。

## 表达效果

原文：

> 对每条支付回调，首先验证签名，再检查签名是否覆盖 order_id 和 amount 字段。只有这两项检查都通过，并且订单当前状态为 pending 时，才允许将订单状态改成 paid。其他情况不得修改订单。

巴别语表达示意：

```text
∀支付回调:
  顺序: 验签→检查{order_id, amount}⊆签名覆盖字段
  仅当两项检查通过∧订单状态=pending: 允许订单状态pending→paid
  否则: 禁止修改订单
```

目标是保留信息并减少 Token，具体幅度取决于输入、模型和编码。Babel 提供文本 Token 计数和语义审计；代理只采用本次审计通过且所选计数更少的候选，否则保留原文。示意不代表实测模型输出，审计通过也不等于语义无损保证。

长文本见[完整售后 SOP 与巴别语参考](docs/examples/README.md)。检测到候选复刻原文多个编号标题时，最多额外重组一次；仍复刻则明确失败。

## 怎么用

需要 **Go 1.22+**，以及一个 OpenAI Chat Completions 兼容的编译模型服务。

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

直接启动后，程序从 `127.0.0.1:8080–8099` 选择可用端口并尝试打开浏览器。在「模型配置中心」保存请求 URL、API Key 和模型名，然后粘贴文本编译。**完整端点按填写值原样发送 POST**（路径、末尾斜杠、查询参数都保留）；只填到主机或以 `/v1` 结尾的基址会自动补 `/chat/completions`，避免 `POST /v1` 这类必然 404 的配置。配置字段仍叫 `base_url`。

专业词典在「领域表达库」新建，也可将内置示例「复制为新词典」后修改、保存，再选中使用。目标计数默认是估算；确认消费模型编码后，可使用 `cl100k_base` / `o200k_base` 精确文本计数。

### 命令行

保存好模型配置后：

```bash
# 编译文本并执行严格审计
./bin/babel -gate "你的提示词"

# 从文件读取，只输出通过审计的候选
./bin/babel -gate -q -f prompt.md > compiled.txt

# 可选：指定领域；其余参数见 -h
./bin/babel -domains api-contract -gate -f spec.md
```

### HTTP API 与代理

```bash
./bin/babel -serve 127.0.0.1:8080
```

在另一个终端调用：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"你的提示词","strict_gate":true}'
```

代理使用前，在模型配置中设置最终服务的基址 `upstream_url`，代理按协议构造转发路径。OpenAI 代理留空时原样复用编译请求 URL；Anthropic 代理须填写该基址或 `X-Upstream-BaseURL`。**配置中的模型用于编译，请求体中的 `model` 用于最终执行。**

客户端可连接 `/v1/chat/completions`（OpenAI，使用 `Authorization`）或 `/v1/messages`（Anthropic，使用 `x-api-key`），携带最终服务的认证信息。最终上游须支持所用协议；如需领域词典，用 `X-Babel-Domains` 传入逗号分隔的名称。代理不会额外翻译模型响应，原任务要求的回答语言和格式应保留在提示词中。

当前服务默认面向本机使用。更多用法：[领域表达库](docs/usage/domain-library.md) · [Token 计数](docs/usage/token-counting.md) · [验证模式](docs/usage/validation.md)。

## License

[MIT](LICENSE)
