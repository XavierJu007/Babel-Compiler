# 领域表达库使用指南

领域表达库提供公开术语、符号及使用边界；输出预设决定表达形式。两者可以独立组合。专业词典由使用者编写和维护，Babel 负责加载与编译，内置专业条目作为编辑示例。
例如，同一份 API 说明可以选择 `api-contract` 领域，再用 `standard` 输出规则列表，或用 `token` 输出简洁文本。

本页描述当前接口和配置方式。词条示例是表达参考，尚不代表真实 tokenizer 节省或跨模型语义验证结果。

## 1. 通用规则、内置示例与默认选择

| 名称 | 词条数 | 内容 |
| --- | --- | --- |
| `common` | 4 | 条件与逻辑关系、事实否定与规范模态、集合成员与空集合、带类型关系与比较 |
| `application-security` | 3 | 安全词典示例：授权、证据状态、操作范围与副作用 |
| `api-contract` | 3 | API 词典示例：接口契约字面量、幂等与重试、字段存在性与空值 |
| `concurrency` | 2 | 并发词典示例：原子条件更新、操作顺序与终止 |

共四个领域、12 个词条，内置内容见 [domains_builtin.json](../../babel-go/domains_builtin.json)。

- `common` 自动加入，不需要手动填写。
- 未指定领域时，通常只使用 `common`。
- `code-audit` 预设在未指定领域时，额外使用 `application-security`。
- 一旦显式指定领域，就以该列表为准，再加上 `common`。例如 `code-audit + api-contract` 不再自动附带安全领域；需要两者时应同时填写。
- 预设包括 `extreme`、`token`、`standard`、`code-audit`、`en-terse`，以及允许混合语言与公共表达的 `mixed`。领域名称不再用来替代输出预设。

公共语义规则始终参与编译，包括不新造私有码本、保留条件与模态、保护技术字面量。编辑领域或预设不会移除这层规则。

## 2. CLI

以下示例假设已构建 `./bin/babel` 并配置编译模型。领域列表和预设列表不需要调用模型。

```bash
# 查看领域及当前来源
./bin/babel -list-domains

# 查看输出预设
./bin/babel -list-dialects

# 同时使用应用安全与 API 契约；所有选项放在输入参数之前
./bin/babel -m standard -domains application-security,api-contract -gate -f spec.md

# 使用并发领域和简洁文本预设
./bin/babel -m token -domains concurrency -f concurrency.md

# code-audit 未指定领域，默认加入 application-security
./bin/babel -m code-audit -f audit.md
```

`-gate` 要求本次检查通过，否则返回错误；它不表示已经完成独立盲读或跨模型验收。
领域可以逗号分隔，名称区分大小写，使用表中的小写名称；空项目（例如末尾多写一个逗号）会报错。

## 3. HTTP 编译与透明代理

直接编译使用 `POST /api/compile`，通过 `domains` 数组选择领域：

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  --data '{"text":"登录成功后仍须校验用户对订单的删除权限。","mode":"standard","domains":["application-security","api-contract"],"strict_gate":true}'
```

透明代理在原有模型请求中增加两个请求头；请求体仍使用对应上游协议的格式：

```text
X-Babel-Dialect: standard
X-Babel-Domains: application-security,api-contract
```

`X-Babel-Dialect` 选择预设，`X-Babel-Domains` 选择领域。后者是逗号分隔的名称列表，适用于 OpenAI 与 Anthropic 代理入口。
未知预设或未知领域返回明确错误；代理不应把拼错的选择静默替换成默认值。
未传领域列表时，默认规则与 CLI 相同。代理未传预设时使用 `token`。

透明代理可设置 `X-Babel-Dialect: mixed`，允许发给消费模型的文本中英混用并采用公共符号；无需修改客户端显示的原始输入，也不需要给消费模型附加词典。原任务的回答语言和格式要求仍须保留；代理直接透传模型响应，不额外回译。若需要固定回答语言，请在原有任务或系统提示中明确约定。

## 4. 创建或编辑领域

Web 界面中的“领域表达库”可以管理词条、符号、适用条件、不可混淆项和输入/输出示例，选择全局或当前工作区后保存。
编译区域单独选择领域和输出预设；保存领域不会自动改变每一次请求的选择。

从零开始可点“新建领域”；想参考已有内容，先选中示例或自己的词典，再点“复制为新词典”。复制会保留当前编辑内容，创建新标识、版本为 1 的未保存草稿，不改动原词典；修改名称、词条和保存位置后保存，再在编译区域选择它。复制稿不能使用已有词典的标识覆盖原内容；需要编辑原词典时直接选中原项。

也可以直接编辑 JSON 文件。下面是一份完整可用的领域定义示例：

```json
{
  "schema_version": 1,
  "name": "sop-approval",
  "label": "审批规范",
  "version": 1,
  "entries": [
    {
      "id": "amount-threshold",
      "canonical": "金额审批条件",
      "aliases": ["审批阈值"],
      "triggers": ["报销", "审批"],
      "use_when": ["原文明确规定金额与审批要求时"],
      "preserve": ["金额", "币种", "比较边界", "审批人", "要求强度"],
      "do_not_merge_with": ["至少与超过", "必须与建议"],
      "symbols": [
        {"symbol": ">=", "meaning": "大于或等于，包含边界"}
      ],
      "examples": [
        {
          "input": "报销金额大于或等于5000元时，必须由主管批准",
          "output": "报销金额 >= 5000 元时，须主管批准"
        }
      ]
    }
  ]
}
```

| 字段 | 用法 |
| --- | --- |
| `schema_version` | 当前必须为 `1` |
| `name` / `label` / `version` | 领域标识、显示名称、正整数版本 |
| `entries` | 1–128 个词条 |
| `id` | 领域内唯一的内部标识，不进入最终输出 |
| `canonical` | 公开术语或关系名称，必填 |
| `aliases` / `triggers` | 用于匹配当前文本；不是无条件字符串替换表 |
| `use_when` | 至少一条适用条件 |
| `preserve` | 原文已有时必须保留的信息，不用于补充新要求 |
| `do_not_merge_with` | 容易混淆、不可合并的概念或关系 |
| `symbols` | 引用受支持的公共 `symbol` 与固定 `meaning`；业务适用范围写在 `use_when` |
| `examples` | `input` 与 `output`；须保留相同条件、范围和义务强度 |

`aliases`、`triggers`、`preserve`、`do_not_merge_with`、`symbols`、`examples` 都可省略。
`source` 与 `file_path` 是列表返回的来源元数据，无需自行填写。文件不支持未知字段。

API、UI、btw 等公开缩写可写在术语、别名或示例中，并说明适用语境；它们不需要作为新的运算符加入 `symbols`。缩写是否等义、能否脱离词典理解，以及是否减少 token，仍需看实际文本。

### 公共符号的边界

界面从公共符号目录选择符号并填写其固定含义。保存和加载时，后端拒绝未知符号、私有段落代号以及对公共符号的改义；例如不能定义 `A17 = 第三段第二句`，也不能把 `∧` 改成“删除订单”。当前支持的目录由 `GET /api/domains` 的 `public_symbols` 返回，源码位于 [public_symbols.json](../../babel-go/public_symbols.json)。

同一个符号可以用于多个领域，但公共含义保持一致，具体对象和使用条件写在词条中。项目开发时可扩充公共符号目录；新术语、业务范围和示例仍通过领域词条编辑。这些格式检查不能证明任意术语或模型输出都自解释，因此语义审计仍需检查输出是否依赖私有码本。

### 保存位置与覆盖顺序

将上述内容保存为 `sop-approval.json`，文件名必须与 `name` 一致：

| 保存范围 | 位置 | 作用 |
| --- | --- | --- |
| 全局 | `~/.babel_domains/sop-approval.json` | 同一用户下的各工作区可用 |
| 工作区 | `./domains/sop-approval.json` | 仅当前进程的工作目录生效 |

工作区是启动 Babel 时的当前目录，不是可执行文件所在目录。
同名领域按 **内置 → 全局 → 工作区** 覆盖，覆盖的是整份领域定义，不是按词条追加。
想扩展内置领域时，先复制整份定义，再编辑并用同名保存；新建独立领域则使用新名称。

领域名称须以小写字母开头，仅含小写字母、数字、`-`、`_`，最长 64 字节。
文件须为普通 UTF-8 JSON 文件，最大 256 KiB；每个词条最多 16 个符号、4 个示例。
API 保存采用临时文件后替换。直接编辑文件时，保存后需重载；再次启动 CLI 也会重新加载。

## 5. 管理 API 与重载

| 请求 | 请求体 | 用途 |
| --- | --- | --- |
| `GET /api/domains` | 无 | 从磁盘重载并返回当前领域列表 |
| `POST /api/domains` | `{"action":"reload"}` | 重载 |
| `POST /api/domains` | `{"action":"save","scope":"workspace","domain":{...}}` | `domain` 放完整领域对象；保存并重载 |
| `POST /api/domains` | `{"action":"delete","scope":"workspace","name":"sop-approval"}` | 删除指定范围的覆盖文件并重载 |

表中 `{...}` 是完整领域对象的占位说明，不能原样发送。保存到全局时将 `scope` 改为 `global`。

```bash
curl http://127.0.0.1:8080/api/domains

curl http://127.0.0.1:8080/api/domains \
  -H 'Content-Type: application/json' \
  --data '{"action":"reload"}'
```

删除只处理指定范围。删除工作区覆盖后，可能重新显示全局版本；删除全局覆盖后，可能恢复内置版本。
内置领域不会从程序中删除；没有可删除覆盖文件时，删除不会移除内置定义。

## 6. 哪些词条会进入编译请求

选择领域后，编译器按词条的术语、别名、触发词及已有符号与当前文本的匹配情况挑选资料。
没有匹配的词条不注入；英文单词匹配考虑边界，例如 `or` 不会命中 `order`。当前实现没有语义检索或自动推断整个业务领域。

每次最多选取 **8 个词条、6000 字符的领域资料**，这是资料预算，不是目标模型 token 预算。
词条过长或达到预算时会跳过，公共语义规则仍保留。需要稳定应用的要求应直接写在原文中，不应只放在可能未匹配的词条里。

符号须保持公共含义。例如 `∈` 表示集合成员，`∅` 仅表示空集合；禁止把 `∅` 同时定义为 null、字段缺失和未知。
“未执行”与“禁止执行”、“必要条件”与“充分条件”、“原因”与“顺序”必须区分。词典不能用 `A17` 代表另一段文字，也不能要求消费模型查图例。

## 7. 错误、迁移与检查范围

坏 JSON、字段无效、名称不匹配或超出限制会显示错误或加载警告。
如果某个选中领域存在坏覆盖文件，编译会拒绝使用它，不静默回到内置定义；未选择的坏领域不会阻止其他合法领域编译。
管理 API 的 `warning` 可能与合法领域列表同时返回，请据警告修复文件，再重载。无效操作和不支持的请求方法也返回明确错误。

旧版自动释放的五份默认预设会按已知内容哈希识别：未编辑副本不再挡住新版内置规则，用户编辑过的副本仍保留。
新导出的默认预设带内容哈希标记，未编辑时跟随内置版本，编辑后成为用户覆盖；重载不再自动向磁盘释放默认文件。
领域 JSON 与输出预设 Markdown 分别管理；修改领域内容会改变组合编译身份，避免继续使用旧组合的候选缓存。

实现位置：[领域结构、校验与选择](../../babel-go/domains.go)、[预设与公共规则组合](../../babel-go/dialects.go)、[领域管理 API](../../babel-go/domain_api.go)。
设计背景与后续验证方向见 [语义图表达与领域词典设计](../design/semantic-compression.md)。

