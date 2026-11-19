# 离线案例与模型评测入口

这是一个小型、可审阅的语义回归包：30 个案例（15 中文、15 英文），每例包含一个拟定合法候选、两个定向反例和关键关系。
所有案例均由助手起草，标记为 `assistant_drafted_requires_human_review`，尚未人工复核，也未运行真实模型评测。

## 三类结果需要分开

| 结果 | 能说明什么 | 不能说明什么 |
| --- | --- | --- |
| 离线结构检查通过 | JSONL 合法、ID 唯一、反例关系引用有效、保护字面量存在 | 候选语义等价、标签正确、token 更少 |
| mock 回归测试通过 | 工具正确处理预算、参数、响应、错误和统计 | 编译器或真实模型的语义能力 |
| 显式联网运行记录 | 指定 Babel API 对实际请求的返回结果 | 自动证明上游模型身份、普遍语义正确率 |

案例覆盖条件与原因、必要与充分条件、当且仅当、模态强度、多条否定、量词范围、阈值与单位、数值绑定、先后与并行、null/空值/缺失、字面量与空格、私有码本、已有变量，以及领域知识误补。
部分候选为避免丢失约束仍较长；这里没有预先测量或承诺压缩率。

## 文件与 schema

- [cases.jsonl](cases.jsonl)：一行一个案例，`schema_version=1`。
- [babel-eval](../cmd/babel-eval/main.go)：Go 命令入口；默认离线检查，`report` 子命令生成可搜索、筛选的离线 HTML 对照报告。
- [evalrun](../internal/evalrun/)：评测实现与行为测试，只使用 Go 标准库；单测使用本地 mock。
- [service_integration_test.go](../internal/evalrun/service_integration_test.go)：可选的实际 Babel HTTP 集成，上游全部为本地 fixture。

| 字段 | 含义 |
| --- | --- |
| `id` / `language` / `domains` / `tags` | 稳定标识、语言、编译时领域选择、覆盖类别 |
| `annotation_status` | 当前固定为助手草拟、待人工复核 |
| `source` | 原文 |
| `valid_candidate` | 拟定的合法紧凑表达，不是模型实测输出 |
| `expected_relations` | `{id, statement}` 列表，记录关键实体、条件、范围或关系 |
| `protected_literals` | 在原文与拟定合法候选中都必须逐字出现的片段 |
| `invalid_candidates` | `{id, text, violated_relations, reason}` 列表，说明定向改变及关联关系 |

这些标签不是独立真值。人工复核时，应先核对 `source` 与关系清单，再分别判断合法候选和反例。
如果发现原文歧义、标签不成立或存在未标注变化，先修订案例并保留版本记录，不能为了让某个模型通过而改标签。

## 1. 默认离线运行

在仓库根目录执行：

```bash
go run ./cmd/babel-eval
go test ./internal/evalrun
```

第一条命令检查完整案例集，输出结构检查结果和数据文件 SHA-256；没有网络请求。
第二条命令使用模拟 API 返回值检查工具行为，也不访问上游模型。报告前端的筛选行为测试在本机有 Node 时运行，没有 Node 时只跳过这一项。
离线检查不会用文本相似度或“数字仍在”推导语义得分。

## 2. 显式调用真实 Babel 服务

先启动 Babel，并在产品中配置需要使用的模型方案。评测器只把显式 `--profile` 名称交给服务，不读取模型配置文件、环境 API Key 或 `/api/config`。
以下方案名只是占位示例，须替换成用户自行选择的已有方案；未知方案由服务拒绝。

联网运行必须同时指定 `--live`、`--endpoint`、`--profile`、`--max-requests` 和新的 `--output` 文件。
`--endpoint` 是 Babel 服务基地址，不是上游模型服务地址。评测器不跟随重定向，不自动重试。
已有输出文件不会覆盖，以免混淆不同运行。

### 连续生成并验证实际候选

`pipeline` 先编译原文，再把实际生成的文本逐字交给验证接口。不使用案例中的拟定候选替代编译结果，也不向模型提供草拟标签、预期关系或反例原因。

```bash
go run ./cmd/babel-eval --live pipeline \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-compiler \
  --validation-mode blind \
  --token-encoding o200k_base \
  --limit 2 --max-requests 4 \
  --output eval-runs/pipeline-pilot.jsonl
```

默认使用同一方案编译与对照；可另加 `--validation-profile chosen-comparator` 选择比较器，`--reader-profile chosen-reader` 选择盲读阅读器。目标编码应按最终消费模型选择，不由编译器名称推断。

每例最多两次 Babel HTTP 请求：生成一次、验证一次，共用 `--max-requests`。完整的 blind 链路通常包含三次上游模型调用；HTTP 请求预算不等于模型调用或费用预算。编译失败不进入验证，预算耗尽时保留已生成但未验证的候选及未开始的案例，标为 `not_validated`，不会自动重试或补跑。

每例记录生成与验证的 HTTP 状态、耗时、实际候选、计量来源和验证明细。仅在验证通过且候选计数更少时，评测规则才选择候选，否则按原文回退计算文本量。`selection_scope="evaluation_rule_only"` 明确表示这是评测中的选择规则：**没有把候选发送给最终消费模型，也没有测量实际账单。**

### 生成候选

```bash
go run ./cmd/babel-eval --live compile \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-compiler \
  --limit 2 --max-requests 2 \
  --token-encoding cl100k_base \
  --output eval-runs/compile-pilot.jsonl
```

调用 `POST /api/compile`，对原文生成候选，发送 `verify=false`、`strict_gate=false`、`no_cache=true`。
领域取自案例，使用产品固定的 `mixed` 表达规则；旧 `--mode` 参数仅保留用于兼容既有命令，不用于选择不同输出风格。拟定候选、反例和预期关系不会发送给编译器。
`--token-encoding` 可选 `estimate`、`cl100k_base`、`o200k_base`；不填写时继承所选产品方案。
选定编码的精确文本计数仍不包含消息封装或工具 schema，也不等于其他模型的 tokenizer 结果。

### 对照已有候选

```bash
go run ./cmd/babel-eval --live validate \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-comparator \
  --validation-mode compare \
  --limit 1 --max-requests 3 \
  --output eval-runs/compare-pilot.jsonl
```

调用 `POST /api/validate`，每例依次提交拟定合法候选和两个反例，不重新编译。
请求仅包含原文、候选、方案及验证模式，不发送草拟标签、反例原因或预期关系。
`compare` 为原文与候选对照；服务返回的 `validation.method`、`policy_version` 等信息会记录。

### 独立阅读模式

```bash
go run ./cmd/babel-eval --live validate \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-comparator \
  --validation-mode blind --reader-profile chosen-reader \
  --case-id zh-null-empty-missing \
  --limit 1 --max-requests 3 \
  --output eval-runs/blind-pilot.jsonl
```

`blind` 请求交由服务执行独立阅读和后续对照；评测器本身不实现或模拟阅读器。
不传 `--reader-profile` 时继承所选方案保存的阅读器设置；传 `--reader-profile chosen-reader` 时覆盖为指定方案。
显式 `--reader-profile=` 会发送空字符串，清除本次请求继承的阅读器覆盖，改用 `--profile` 指定的方案执行阅读；不会修改已保存的配置。
这个参数（包括显式空串）只允许用于 `--live validate` 或 `--live pipeline` 的 `blind` 模式；pipeline 中清空覆盖后，阅读器使用本次验证方案。
评测器核对返回的执行方式和协议版本：`blind_read` / `semantic-blind-v1`，以及 `local_checks`、`reader`、`comparator` 三个阶段各自的当前版本。
只有本地检查 `completed`、阅读器 `completed` 且附有 `relations` 非空、`ambiguities=[]` 的结构化阅读回执、对照器 `passed` 时，才把顶层 `passed` 计为独立阅读通过；`entities` 可以为空。
在该模式下收到普通 `compare`、旧协议、缺失阶段或互相矛盾的状态，一律记为 `unknown`，不会降级为通过。
一次验证请求可能涉及阅读与对照两次上游模型调用。`--max-requests` 限制的是 HTTP 请求数，不是上游调用次数、token 或费用上限。

## 3. 选择案例与控制预算

默认只运行文件顺序中的前 **2 个案例**，不是自动运行全量。
`--case-id` 可重复指定；筛选后仍按文件顺序取前 `--limit` 个案例。
编译模式每例最多一个 HTTP 请求；当前验证数据每例三个请求；pipeline 每例最多两个请求，编译失败会省去该例验证。
计划请求数超过 `--max-requests` 时，执行到预算即停止，余下数量记为 `skipped_by_budget`；不会自动补跑。

输出是逐行写入的 JSONL：运行说明、每次请求观察、最终汇总。
记录含原文、实际候选、预期关系、HTTP 状态、耗时及服务返回的计量元数据；异常的上游响应正文不复制进报告。
`--timeout` 默认每个请求 60 秒。错误会单独记录，不会通过自动重试突破预算。

## 4. 如何阅读结果

`compile` 仅记录生成结果与保护字面量的出现情况，`semantic_quality` 保持 `not_evaluated`。
服务提供的 `measurement_kind/source/scope/encoding/version` 原样保留，评测器不把估算 token 改称实测账单。

`validate` 的 `draft_label_confusion_matrix` 将草拟合法/非法标签与服务 `passed/failed` 结果交叉统计。
普通对照只接受 `semantic-gate-v2` 下的 `llm_compare` 回执，或本地检查明确拒绝的 `local_checks` 回执。
独立阅读允许本地检查拒绝、阅读器拒绝或最终对照拒绝，但后续未运行的阶段必须与拒绝位置一致；阅读器完成本身不算通过。
每条观察记录包含 `requested_mode`、`receipt_check` 和阶段元数据，便于定位协议不匹配。协议升级后须先同步评测器的版本约定，不能仅凭顶层状态继续计分。
`draft_label_agreement` 是这两类已决结果与草拟标签的一致比例，名称和含义均是 **draft-label agreement**，不是事实语义正确率。
`error` 和 `unknown` 单列并排除在一致比例分母之外，不能算作“成功拒绝反例”；没有已决结果时比例为 `null`。
HTTP 502 等非 200 响应始终计为 `error`；HTTP 200 中明确的服务或阶段 `error` 也不计入语义失败。报告保留状态信息，省略异常详情中的上游原始正文。
`api_success` 只表示协议层产生可用结果，不代表候选正确。

`pipeline` 统计的是 `validation_pass_rate = passed / (passed + failed)`，即所选验证流程在已决结果中的通过率；本地检查提前拒绝也属于 `failed`。`error`、`unknown`、`not_validated` 的数量同时单列，不能隐藏这些样本后只展示通过率。pipeline 不计算与拟定合法/非法标签的一致比例，因为它验证的是本次生成的文本。

pipeline 的 `measurement_groups` 按 `kind/source/scope/encoding/version` 完整组合分组，分别记录原文量、候选量、膨胀/缩短/不变数量，以及按规则回退后的有效文本量。不同编码和估算/精确结果不相加；无来源、非法计数或互相矛盾的元数据会被排除并记录原因。即使某候选很短，只要验证失败，它在回退后计量中的节省就是零。未知计量的案例不会悄悄填零混入统计。

运行记录始终标明上游模型身份未独立核验。`--live` 代表启用网络；若 endpoint 指向 mock，它仍然只是 mock 结果。
当前仓库没有随附真实模型分数。正式比较应在人工复核后冻结案例版本，固定模型、验证策略与编码，并保留失败案例；不能边修改案例边报告同一集合上的“提升”。

退出码：`0` 表示结构检查或联网记录正常结束（即使存在草拟标签分歧）；`1` 表示输入、配置或文件错误；`2` 表示联网结果存在 API 异常或未知验证状态。

## 5. 生成离线 HTML 报告

```bash
go run ./cmd/babel-eval report eval-runs/pipeline-pilot.jsonl \
  --output eval-runs/pipeline-pilot.html
```

报告支持已有 compile/validate 记录及 pipeline 记录。直接在浏览器打开 HTML 即可搜索文本、筛选结果，逐例并排查看原文与实际候选，展开问题和盲读结果，核对生成、验证及阅读阶段的耗时。内容全部内嵌，不加载外部脚本或字体，也不会访问模型服务。

报告会显示运行是否完整、样例是否仍待人工复核、验证方式和计量依据。缺少最终汇总的记录会标为部分运行，不补造完成结果。明显矛盾或不匹配的验证回执不能显示为通过；原始问题与阶段证据保留以便审阅。生成器拒绝覆盖已有输出。

## 6. 实际 HTTP 集成检查

普通 Go 单测使用 mock；实际服务集成需要显式提供已经构建的 Babel 二进制。下面的测试会自行启动临时本机服务和模拟模型，使用临时配置，不使用个人密钥：

```bash
go build -o bin/babel ./babel-go
BABEL_TEST_BINARY="$PWD/bin/babel" go test ./internal/evalrun -run TestRealService -count=1
```

PowerShell 可先用 `go build -o bin/babel.exe ./babel-go` 构建，再设置 `$env:BABEL_TEST_BINARY = (Resolve-Path .\bin\babel.exe).Path`，然后运行同一条 Go 测试命令。未设置该变量时，只跳过实际服务集成测试，其余离线测试正常执行。

集成检查覆盖实际候选逐字传入阅读器、独立方案路由、精确计量元数据、共享请求预算及生成 HTML 报告。它验证产品和评测器的配合；上游回答仍为 fixture，不构成真实模型语义效果证据。

