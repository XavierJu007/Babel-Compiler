# Babel-Compiler

![Babel 二次元少女主题海报](docs/assets/babel-poster.png)

**把自然语言提示词编译成高密度巴别语的小工具。**

- **更紧凑**：固定使用混合语言浅树，以公开术语、缩写和符号表达主体、条件与依赖，减少句子展开。
- **自解释**：不使用私有码本，输出无需额外图例；专业领域词典由你自己编写。
- **可校验**：提供文本 Token 计数与等义审计；代理只采用本次审计通过且更短的候选，否则保留原文。

## 表达示意

> 每笔支付回调先验签，再检查签名覆盖 order_id 与 amount。仅当两项检查通过且订单状态为 pending 时，才可改为 paid；否则不得修改订单。

```text
∀支付回调
├flow: 验签→检查{order_id, amount}⊆签名覆盖字段
├仅当两项通过∧状态=pending: 允许pending→paid
└else: 禁止修改订单
```

示意非实测输出。压缩幅度取决于输入、模型和编码，审计通过也不等于无损保证。[查看完整售后 SOP 示例](docs/examples/README.md)。

## 快速开始

需要 **Go 1.22+** 和一个 OpenAI Chat Completions 兼容的编译模型服务。

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

启动后自动选择 `127.0.0.1:8080–8099` 的可用端口并尝试打开浏览器。在「模型配置中心」保存请求 URL、API Key、模型名，即可粘贴文本编译；专业词典在「领域表达库」自建。

**完整端点原样发送 POST**；只填主机或以 `/v1` 结尾的基址时兼容补全 `/chat/completions`。远程服务必须使用 HTTPS，HTTP 仅限本机环回。[地址与校验说明](docs/usage/validation.md)。

## 命令行与 API

```bash
./bin/babel -gate "你的提示词"
./bin/babel -gate -q -f prompt.md > compiled.txt
```

用 `./bin/babel -serve 127.0.0.1:8080` 启动固定端口，另开终端调用 API：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"你的提示词","strict_gate":true}'
```

## 透明代理

在模型配置中填写最终服务基址 `upstream_url`，客户端连接本机对应入口：

| 协议 | 入口 | 调用方携带的上游认证 |
| --- | --- | --- |
| OpenAI | `/v1/chat/completions` | `Authorization` |
| Anthropic | `/v1/messages` | `x-api-key` |

**编译使用已保存方案，最终执行使用请求体中的 `model`。** 上游 Key 必须由调用方提供，代理不会借用编译 Key。

代理压缩 `system`、`user` 和工具描述，只替换对应文本，保留消息角色与工具结构；模型响应直接透传，不额外回译。[完整接入与回退规则](docs/usage/validation.md#4-代理使用)。

## 文档

[领域表达库](docs/usage/domain-library.md) · [Token 计数](docs/usage/token-counting.md) · [设计说明](docs/design/semantic-compression.md) · [传输安全](docs/security/transparent-proxy-review.md) · [路线图](docs/plans/ROADMAP.md) · [MIT License](LICENSE)
