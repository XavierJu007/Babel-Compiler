# Babel-DSL (Babel-Compiler)

> 面向大语言模型的「巴别语」高密度提示词编译器 —— 在**无损保留业务逻辑与语义约束**的前提下，把冗长的自然语言压缩为结构化符号指令。

自然语言提示词有大量语法噪音：「首先」「其次」「需要注意的是」「如果……那么……」。这些词不携带业务信息，却持续消耗 Token，并稀释模型注意力（Attention）对**条件分支、因果映射、遍历逻辑**的聚焦。

Babel-DSL 把这些结构重新编码为**数理逻辑符号 + 树状缩进 + 结构化分隔符**，让同样的语义占用更少的 Token、呈现更强的结构信号。

```
自然语言 (High Token, Low Signal)         巴别语 (Low Token, High Signal)
──────────────────────────────────       ──────────────────────────────────
首先校验用户名和密码是否匹配数据库；        auth.login
如果连续失败 5 次，则锁定账号 30 分钟，      ├ 校验: cred.user + cred.pwd ∈ db.records
并记录审计日志。                          ├ ? fail_cnt ≥ 5 → 锁 account 30min
不要在前端暴露具体的失败原因。              │  ╰ + 记 audit.log
                                        ╰ ∀ resp: ¬ 暴露 fail_reason ⚠
```

> 上例为效果示意，实际输出由所选模型按系统提示词生成。

## 核心特性

### 已实现

| 能力 | 说明 |
| --- | --- |
| **双语模式编译** | `standard`（标准树状结构）/ `extreme`（极限数理符号），两套系统提示词分别面向可读性与最高密度 |
| **单二进制分发** | Go 实现，前端 `index.html` 通过 `go:embed` 打包进可执行文件，无外部静态资源依赖 |
| **双形态使用** | 同一个二进制既是 CLI，也是本地 Web 服务（无参数启动即拉起 UI） |
| **语义无损校验** | `-v` 开关触发二次 LLM 调用，仅凭巴别语还原约束并与原文比对，输出 `[PASS]` 或缺失点列表 |
| **压缩率统计** | 每次编译返回原始字符数 / 编译后字符数 / 压缩率，UI 以徽标实时展示 |
| **OpenAI 兼容** | 任意兼容 `/chat/completions` 的服务均可接入（OpenAI、DeepSeek、Moonshot、本地 vLLM / Ollama 等） |
| **配置持久化** | 环境变量、CLI 参数、Web 表单三处配置，落盘于 `~/.babel_config.json`（权限 `0600`） |

### 路线图

- **Token 级计量**：接入 tiktoken / 各家 tokenizer，把当前的字符压缩率升级为真实 Token 压缩率面板
- **模型路由**：按文本领域与目标上下文窗口自动选择编译模型与压缩档位
- **预设领域词典**：安全规范、SOP、API 契约、法律条款等领域专属符号表，减少模型自由发挥

## 快速开始

### 构建

```bash
git clone git@github.com:XavierJu007/Babel-Compiler.git
cd Babel-Compiler

# 推荐：静态构建（macOS 上若遇 dyld 报错，务必带上这两个参数）
CGO_ENABLED=0 go build -ldflags="-linkmode=internal" -o bin/babel ./babel-go

# 或直接运行
go run ./babel-go -h
```

### 配置模型

任选一种方式：

```bash
# 1) 环境变量
export BABEL_API_KEY="sk-..."
export BABEL_BASE_URL="https://api.deepseek.com/v1"   # 默认 https://api.openai.com/v1
export BABEL_MODEL="deepseek-chat"                     # 默认 gpt-4o-mini

# 2) 写入本地配置（同时支持前端「保存默认配置」按钮）
./bin/babel -save-config -api-key sk-... -base-url https://api.deepseek.com/v1 -model deepseek-chat
```

优先级：**CLI 参数 > `~/.babel_config.json` > 环境变量 > 内置默认值**。

### CLI 用法

```bash
# 直接传参
./bin/babel "如果检测到 SQL 注入特征，则阻断请求并记录源 IP"

# 管道
cat spec.md | ./bin/babel -m standard

# 文件 + 无损校验（默认 extreme 模式）
./bin/babel -f spec.md -v

# 静默模式：只输出巴别语，便于管道串联
./bin/babel -q -f spec.md > compiled.babel

# 启动 Web UI / HTTP API
./bin/babel -serve :8080
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-m` | `extreme` | 压缩模式：`standard` \| `extreme` |
| `-v` | `false` | 开启双向语义无损校验 |
| `-q` | `false` | 静默模式，仅输出纯文本 |
| `-f` | — | 从文件读取输入 |
| `-serve` | — | 启动服务，如 `:8080` |
| `-api-key` / `-base-url` / `-model` | — | 单次覆盖模型配置 |
| `-save-config` | `false` | 将当前配置写入 `~/.babel_config.json` |

无参数且无管道输入时，自动在 `127.0.0.1:8080` 拉起本地 Web 界面。

### Web 界面

```bash
./bin/babel            # 等价于 -serve 127.0.0.1:8080
```

左右分栏：左侧粘贴自然语言，右侧实时显示巴别语与压缩率徽标；勾选「无损审计」后，下方展开审计结果；顶部可填写 Base URL / API Key / 模型并保存为默认配置。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/config` | 读取当前配置（API Key 仅返回前 6 位掩码） |
| `POST` | `/api/config` | 保存配置到 `~/.babel_config.json` |
| `POST` | `/api/compile` | 编译文本 |

```bash
curl -s http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"逐条校验每个上传文件的扩展名，命中黑名单则拒绝","mode":"extreme","verify":true}'
```

```json
{
  "compiled": "upload.check\n╰ ∀ file → ? ext ∈ blacklist → reject ⚠",
  "audit": "[PASS] 语义100%无损",
  "stats": {
    "original_chars": 25,
    "compiled_chars": 46,
    "compression_ratio": "-84.0%"
  }
}
```

> `compression_ratio` 为负数表示编译后变长。短句、强结构化输入通常不会压缩，甚至膨胀；该指标对**长文档型提示词（SOP、安全规范、接口契约）**才具备参考意义。

## 语义无损校验（Round-Trip Verification）

高密度符号的风险是**过度缩写**：模型为了压缩，悄悄丢掉一个边界条件或阈值。校验开关用一次独立的往返来兜住这个风险：

```
原文 ──► LLM A（编译，temp=0.1）──► 巴别语
                                      │
                                      ▼
                          LLM B（仅见巴别语，temp=0.0）
                                      │
                                      ▼
                    还原出的约束 ◄── 与原文字面比对
                                      │
                    ┌─────────────────┴─────────────────┐
              [PASS] 语义 100% 无损              列出缺失的约束点
```

要点：

- LLM B **不接触原文**，只从巴别语反推执行步骤，因此能暴露「符号自洽但信息缺失」的问题
- 编译与审计使用不同的 temperature（0.1 / 0.0），降低随机性对判定的干扰
- 审计结果是**建议**而非硬门禁，最终仍需人工复核关键约束

## 符号速查

完整规范见 [`docs/符号映射规范.md`](docs/符号映射规范.md)。

| 类别 | 符号 | 语义 |
| --- | --- | --- |
| 逻辑 | `→` `⇒` | 因果 / 动作流向 / 触发 |
| | `∧` `+` | 逻辑与 / 并列 |
| | `∨` `\|` | 逻辑或 / 枚举 |
| | `¬` `!` | 否定 / 禁止 |
| 量词 | `∀` | 全称 / 遍历 |
| | `∃` | 存在 / 命中 |
| | `∈` `⊆` | 属于 / 包含于 |
| 状态 | `:=` `=` | 定义 / 赋值 |
| | `↔` `⇄` | 互换 / 变更 |
| | `?` | 条件判断 / 探测 |
| | `⚠` `🚫` | 强约束 / 红线 |
| 结构 | `├` `│` `╰` | 树状层级，锚定作用域（Scope） |

树形缩进不只是排版：它让子步骤的约束**在视觉上归属于父节点**，抑制模型把局部条件泛化成全局规则。

## 项目结构

```
Babel-Compiler/
├── babel-go/
│   ├── main.go          # CLI + HTTP 服务 + LLM 调用 + 无损校验
│   └── index.html       # 单页前端，go:embed 打包
├── docs/
│   └── 符号映射规范.md   # 巴别语符号与替代关系
├── go.mod
├── LICENSE
└── README.md
```

## 设计取舍

- **不做本地规则引擎**：符号映射交给 LLM，而非正则替换。自然语言的歧义消解需要语义理解，硬编码规则在真实文本上会迅速失效。
- **单文件后端**：约 300 行 Go，无框架、无外部依赖，便于审计与二次开发。代价是没有并发限流与鉴权，**仅建议在本机或可信内网运行**。
- **配置即密钥**：API Key 以 `0600` 落盘在用户目录，不上传、不写入仓库。

## 贡献

Issue / PR 均欢迎。提交前请确保 `go vet ./babel-go` 通过。

## License

[MIT](LICENSE)
