# 目标 tokenizer 计数

Babel 可以使用指定编码的本地 tokenizer 计算输入、输出文本的 token 数。目标编码与编译模型分开配置：编译模型负责改写，目标编码负责计数，不会根据编译模型名称自动选择编码。

## 1. 选择计数方式

| `token_encoding` | 返回的 `measurement_kind` | 含义 |
| --- | --- | --- |
| `estimate` | `estimated` | 使用现有 `heuristic-v2` 估算，不是 BPE tokenizer |
| `cl100k_base` | `exact` | 使用该编码的公开词表，在本地计算普通文本 token 数 |
| `o200k_base` | `exact` | 使用该编码的公开词表，在本地计算普通文本 token 数 |

默认使用 `estimate`。支持的编码名称可从配置 API 返回的 `token_encodings` 读取。名称会去除前后空白并转为小写；未知名称报错，不会默默回退成估算。

`exact` 只说明**所选编码对这段文本的本地计数**。用户需要确认消费模型使用该编码；Babel 不会把 `cl100k_base` 或 `o200k_base` 自动套用到 Qwen、Claude 等模型，也不接受模型名作为编码名。不确定目标 tokenizer 时，应显式保留 `estimate`。

两种精确编码的词表随 Go 依赖编译进程序。已经构建的程序计数时不联网，不下载词表，也不调用收费模型。首次构建仍需获取 Go 模块依赖。

## 2. 配置和调用

可在 Profile 中设置独立的 `token_encoding`，也可以逐请求覆盖。请求省略该字段或传空字符串时继承 Profile；Profile 未设置时使用 `estimate`。显式传 `estimate` 可以覆盖 Profile 中配置的精确编码。

CLI 选项放在输入参数之前：

```bash
# 目标消费编码已经确认是 o200k_base
./bin/babel -m standard -token-encoding o200k_base -f spec.md

# 本次只做估算，即使 Profile 设了精确编码
./bin/babel -token-encoding estimate -f spec.md
```

HTTP 编译请求：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  --data '{"text":"请求必须经过授权检查。","mode":"standard","token_encoding":"cl100k_base"}'
```

透明代理使用独立请求头，适用于 OpenAI 与 Anthropic 代理入口：

```text
X-Babel-Token-Encoding: o200k_base
```

这个请求头只选择计数方式，不会更改上游 `model` 或编译模型。它也不意味着被代理的模型已经与该编码核对一致。

## 3. 如何解释统计结果

以 `o200k_base` 为例，统计元数据包括：

```json
{
  "measurement_kind": "exact",
  "measurement_source": "tiktoken-go/tokenizer",
  "measurement_scope": "text",
  "measurement_encoding": "o200k_base",
  "measurement_version": "v0.3.0"
}
```

估算方式对应 `measurement_kind: "estimated"`、`measurement_source: "heuristic-v2"`、`measurement_encoding: "estimate"`、`measurement_version: "2"`。

统计范围始终为 `text`，有以下边界：

- 保留原始文本的空格、换行、Unicode 字符后计数，不先做去空白或 Unicode 规范化。
- `<|endoftext|>`、`<|fim_prefix|>` 等看似特殊 token 的用户文本按普通正文计数，不当作模型控制 token；参考行为是官方 `encode_ordinary`。
- 不包括聊天消息边界、角色、工具定义、图片、隐藏系统提示或供应商添加的包装。局部文本计数之和也不保证等于完整请求的分词结果。
- 上游返回的 usage 是整次请求使用量。除非另有同条件原文基线，不能用它声称实测节省了多少 token 或费用。
- 不同编码可能对同一文本给出不同计数。文本变短、符号变少，也不保证 token 更少；计数不能代替语义保留检查。
- 非法 UTF-8、未知编码或 tokenizer 失败会返回明确错误，不把失败包装成 `exact`，也不悄悄换成另一种计数方式。

计数不改变生成提示。相同生成结果可以被缓存或由并发请求共享；每次请求仍按自己选择的编码重新计算统计值，不继承另一请求的编码。

## 4. 实现、数据来源和许可证

计数使用 [`github.com/tiktoken-go/tokenizer v0.3.0`](https://github.com/tiktoken-go/tokenizer/tree/v0.3.0)，依赖 [`github.com/dlclark/regexp2 v1.9.0`](https://github.com/dlclark/regexp2/tree/v1.9.0)。两者为 MIT 许可，版本和校验值固定在项目的 `go.mod`、`go.sum` 中。版权与完整许可声明见 [第三方许可](../THIRD_PARTY_NOTICES.md)。

选择这个版本是为了兼容项目的 Go 1.22：它的 `go.mod` 要求 Go 1.20。更新的 tokenizer 版本引入了 Go 1.22 无法解析的 `tool` 指令或更高 Go 版本要求；这里没有修改上游实现或自行重写 BPE。

两份内嵌词表来自 OpenAI 公布的 BPE 数据。开发时将依赖内嵌的全部 token/rank 按 rank 排序还原成 `.tiktoken` 文件格式，得到的 SHA-256 与官方参考实现指定值一致：

| 编码 | 普通词表项数 | SHA-256 |
| --- | ---: | --- |
| `cl100k_base` | 100256 | `223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7` |
| `o200k_base` | 199998 | `446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d` |

原始文件：[cl100k_base.tiktoken](https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken)、[o200k_base.tiktoken](https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken)。官方编码定义与校验值见 [OpenAI tiktoken 0.12.0](https://github.com/openai/tiktoken/blob/0.12.0/tiktoken_ext/openai_public.py)。

## 5. 离线回归与参考数据再生成

`babel-go/testdata/token_count_vectors.json` 保存独立的官方 Python tiktoken 0.12.0 参考结果，覆盖 29 条文本、2 种编码。包括中文、英文、日文、阿拉伯文、数学符号、组合字符、emoji、连续空格、换行、控制字符，以及看似特殊 token 的正文。

正常 Go 测试直接读取这些参考值，不需要 Python，不需要联网：

```bash
go test ./babel-go -run TestTokenCounter -count=1
go test -race ./babel-go -run TestTokenCounter -count=1
```

参考数据只在维护 tokenizer 时重新生成。在独立 Python 环境安装 `tiktoken==0.12.0` 后运行：

```bash
python babel-go/testdata/generate_token_count_vectors.py
```

再生成脚本只加载公开词表并执行本地分词，不使用 API key，不调用模型。参考测试验证选定文本与指定版本一致；它不是对所有模型计费规则或任意未来 tokenizer 版本的保证。

## 6. 英文、缩写与语言选择

默认预设保留原文语言。选择 `en-terse` 可在一次编译中将叙述改写为简洁英文，不增加单独的翻译请求；原文要求的回答语言、格式、语气，以及数值、技术字面量和专名仍须保留。例如输入要求“请用中文回答”，英文候选仍需明确要求用中文回答，不能因编译正文是英文就改变任务。

```bash
# 仅在已确认消费模型使用该编码时选择 o200k_base
./bin/babel -m en-terse -token-encoding o200k_base -f prompt.md
```

公开且语境清楚的缩写可以使用，不必为它们另发图例；不能新造代号，也不能把原文的具体条件换成模糊缩写。`btw` 等常用表达不等于通用压缩算子；`ASAP` 不能替代明确的“30 秒内”，`auth` 也可能混淆认证和授权。是否使用由原文含义和上下文决定。

下面是 2026-11-19 使用项目当前 `tiktoken-go/tokenizer v0.3.0` 的本地计数，逐字计数表中独立字符串，不附加空格、换行或消息包装，没有调用模型：

| 原表达 → 改写表达 | `cl100k_base` | `o200k_base` |
| --- | ---: | ---: |
| `by the way` → `btw` | 3 → 2 | 3 → 2 |
| `laughing my ass off` → `lmao` | 6 → 3 | 5 → 2 |
| `with` → `w/` | 1 → 2 | 1 → 2 |
| `for example` → `e.g.` | 2 → 3 | 2 → 3 |
| `application programming interface` → `API` | 3 → 1 | 3 → 1 |
| `禁止删除日志。` → `Do not delete logs.` | 7 → 5 | 4 → 5 |

这是这些字符串的计量示例，不是跨模型语义效果或整体压缩率。放入完整句子后，空格、标点和上下文可能改变分词，应比较实际发送的完整文本。英文或更短的缩写都不保证 token 更少；字符信息密度、语言的信息熵也不能代替目标 tokenizer 的计数。

使用时直接选择适合自己的预设、检查输出和计数即可，不需要自动搜索多种语言。关于分词原理可参考 [tiktoken 官方说明](https://github.com/openai/tiktoken)；关于先转英文的任务与语言差异，可参考 [Beyond English，NAACL Findings 2025](https://aclanthology.org/2025.findings-naacl.73/)。
