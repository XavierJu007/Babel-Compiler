# Babel-Compiler

[English](README.md) | [简体中文](README.zh-CN.md)

![Babel 二次元少女主题海报](babel-go/assets/babel-poster.png)

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

到 **[Releases](https://github.com/XavierJu007/Babel-Compiler/releases/latest)** 下载对应系统的程序包，按下表选择文件后缀，别选 GitHub 自动生成的 **Source code**。程序已内嵌页面和海报，无需安装 Go，也不用下载仓库里的其他文件。

| 系统 | 程序包后缀 |
| --- | --- |
| Windows | 普通 Intel/AMD 电脑：`windows_amd64.zip` · ARM 电脑：`windows_arm64.zip` |
| macOS | Apple Silicon：`macos_arm64.tar.gz` · Intel：`macos_amd64.tar.gz` |
| Linux | x86-64：`linux_amd64.tar.gz` · ARM64：`linux_arm64.tar.gz` |

1. 解压程序包。**Windows：**双击 `babel.exe`，使用期间保持控制台窗口打开。**macOS/Linux：**在解压目录打开终端，运行 `./babel`。
2. 程序自动选择 `127.0.0.1:8080–8099` 的可用端口并尝试打开浏览器；未自动打开时，访问终端显示的地址。
3. 自备一个 OpenAI Chat Completions 兼容的模型服务，在「模型配置中心」保存请求 URL、API Key、模型名，即可粘贴文本编译；专业词典在「领域表达库」自建。

**完整端点原样发送 POST**；只填主机或以 `/v1` 结尾的基址时兼容补全 `/chat/completions`。远程服务必须使用 HTTPS，HTTP 仅限本机环回。[地址与校验说明](docs/usage/validation.md)。

<details>
<summary>从源码构建（开发者，需要 Go 1.22+）</summary>

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

</details>

## 命令行与 API

在解压目录运行以下命令；Windows 将 `./babel` 换成 `.\babel.exe`。用 `./babel -version` 查看版本。

```bash
./babel -gate "你的提示词"
./babel -gate -q -f prompt.md > compiled.txt
```

用 `./babel -serve 127.0.0.1:8080` 启动固定端口，另开终端调用 API：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"你的提示词","strict_gate":true}'
```

## 透明代理

在模型配置中填写最终服务基址 `upstream_url`，客户端连接本机对应入口：

| 协议 | 入口 | 调用方携带的上游认证 | 行为 |
| --- | --- | --- | --- |
| OpenAI Chat | `/v1/chat/completions` | `Authorization` 或 `api-key` | 压缩 `system`/`user` 与工具描述 |
| OpenAI Responses | `/v1/responses` | `Authorization` 或 `api-key` | 压缩 `instructions`/`input` 与工具描述 |
| Anthropic | `/v1/messages` | `x-api-key` 或 `Authorization` | 压缩 `system`/`user` 与工具描述 |
| 其他端点 | `/v1/*`（如 `/v1/models`、`/v1/messages/count_tokens`、`/v1/embeddings`） | 同上 | 零解析流式透传 |

**编译使用已保存方案，最终执行使用请求体中的 `model`。** 上游 Key 必须由调用方提供，代理不会借用编译 Key。

代理只替换对应文本字段，保留消息角色、工具结构、查询参数与业务请求头；模型响应（含长 SSE 流）直接透传，不额外回译。支持 `X-Babel-Dry-Run: true` 零风险观察模式、`X-Babel-Bypass: 1` 单次旁路、`X-Babel-Compile-Budget` 编译延迟预算、`X-Babel-Skip-Cache-Control: true` 保护上游 Prompt Cache、Web 代理策略实时切换、单条日志一键复制脱敏诊断包，以及 `GET /healthz` 健康与指标检查。[完整接入与回退规则](docs/usage/validation.md#4-代理使用)。

### 为什么调用编译器反而能省钱？（成本与回本账）

只有在**编译器远比消费模型便宜**且**压缩前缀被多轮复用**时，编译才有正收益：

1. **便宜/本地免费编译器 -> 昂贵主力消费模型**：编译器和校验器配置本地 Ollama（如 `qwen2.5:7b`，边际成本接近 0）或极低价小模型（`gpt-4o-mini` / `deepseek-chat`），而实际业务/Agent 流量调用 Claude Sonnet/Opus 或 GPT-5。
2. **多轮 Agent 摊薄编译成本**：Claude Code、Codex 与多工具 MCP 对话中，冗长的 `system` 提示词和 `tools` JSON Schema 在几十上百轮里高度重复。Babel 在后台异步预热（`async_warmup`）并验证一次后，后续请求全部走内存双缓存快路径（接近 0 ms 额外延迟，零重复编译调用）。

### Dry-Run 观察模式：零风险先测自己工作流能省多少

首次接入不想立刻改动线上 Prompt？可在 Web「透明代理」顶部的策略栏勾选 **Dry-Run 观察模式**（或环境变量 `BABEL_PROXY_DRY_RUN=true` / 请求头 `X-Babel-Dry-Run: true`）：

- 所有请求 **100% 原样转发给上游**（零语义误改风险、不阻塞首包）；
- 后台照常完成异步压缩测算与等义校验，并在 Web 看板实时汇总：**「预计可省 Token 数、节省比例、命中次数/总次数」**，确认自己工作流有真实收益再一键切到正式压缩。

### 60 秒主流客户端接入配方

1. **Claude Code（Anthropic 协议）**：在模型配置里填好 `upstream_url`（如 `https://api.anthropic.com`），终端执行：
   ```bash
   export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
   claude
   ```
2. **Codex CLI / OpenAI SDK**：
   ```bash
   export OPENAI_BASE_URL="http://127.0.0.1:8080/v1"
   codex
   ```
3. **Ollama 本地零编译成本**：右上角「模型配置中心」填入 BaseURL `http://127.0.0.1:11434/v1/chat/completions`、API Key `ollama`、Model `qwen2.5:7b`（`upstream_url` 仍填你的云端主力模型服务）。
4. **Cursor / Cline / Cherry Studio**：将 Base URL 指向 `http://127.0.0.1:8080/v1`（或 `http://127.0.0.1:8080`）；若客户端附带浏览器 `Origin` 头，启动时加上 `BABEL_PROXY_ALLOWED_ORIGINS="http://localhost:3000"`。

### Docker Sidecar 部署

出于安全边界设计，Babel 仅绑定环回地址（`127.0.0.1:8080`），避免本地凭据与 Prompt 暴露到局域网：

```bash
docker compose up -d --build
```

[docker-compose.yml](docker-compose.yml) 中已包含 `BABEL_PROXY_DRY_RUN=true` 与容器 sidecar（`network_mode: "service:babel"`）注释示例。

### 真实模型基准评测（`babel-eval`）

启动 Babel 并保存可用配置方案后，可直接用内置案例或自定义 JSONL 测试集跑真实模型的压缩率与验证通过率：

```bash
go run ./cmd/babel-eval --live pipeline \
  --endpoint http://127.0.0.1:8080 \
  --profile default \
  --limit 10 --max-requests 20 \
  --output eval/results/live.jsonl
go run ./cmd/babel-eval report eval/results/live.jsonl -output eval/results/live.html
```

更多参数与自建测试集规范见 [eval/README.md](eval/README.md)。

## 文档

[领域表达库](docs/usage/domain-library.md) · [Token 计数](docs/usage/token-counting.md) · [设计说明](docs/design/semantic-compression.md) · [传输安全](docs/security/transparent-proxy-review.md) · [路线图](docs/plans/ROADMAP.md) · [MIT License](LICENSE)
