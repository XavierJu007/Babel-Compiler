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

| 协议 | 入口 | 调用方携带的上游认证 |
| --- | --- | --- |
| OpenAI | `/v1/chat/completions` | `Authorization` |
| Anthropic | `/v1/messages` | `x-api-key` |

**编译使用已保存方案，最终执行使用请求体中的 `model`。** 上游 Key 必须由调用方提供，代理不会借用编译 Key。

代理压缩 `system`、`user` 和工具描述，只替换对应文本，保留消息角色与工具结构；模型响应直接透传，不额外回译。[完整接入与回退规则](docs/usage/validation.md#4-代理使用)。

## 文档

[领域表达库](docs/usage/domain-library.md) · [Token 计数](docs/usage/token-counting.md) · [设计说明](docs/design/semantic-compression.md) · [传输安全](docs/security/transparent-proxy-review.md) · [路线图](docs/plans/ROADMAP.md) · [MIT License](LICENSE)
