# 离线案例与模型评测入口

这是一个小型、可审阅的语义回归包：30 个案例（15 中文、15 英文），每例包含一个拟定合法候选、两个定向反例和关键关系。
所有案例均由助手起草，标记为 `assistant_drafted_requires_human_review`，尚未人工复核，也未运行真实模型评测。

## 三类结果需要分开

| 结果 | 能说明什么 | 不能说明什么 |
| --- | --- | --- |
| 离线结构检查通过 | JSONL 合法、ID 唯一、反例关系引用有效、保护字面量存在 | 候选语义等价、标签正确、token 更少 |
| mock 回归测试通过 | 脚本正确处理预算、参数、响应、错误和统计 | 编译器或真实模型的语义能力 |
| 显式联网运行记录 | 指定 Babel API 对实际请求的返回结果 | 自动证明上游模型身份、普遍语义正确率 |

案例覆盖条件与原因、必要与充分条件、当且仅当、模态强度、多条否定、量词范围、阈值与单位、数值绑定、先后与并行、null/空值/缺失、字面量与空格、私有码本、已有变量，以及领域知识误补。
部分候选为避免丢失约束仍较长；这里没有预先测量或承诺压缩率。

## 文件与 schema

- [cases.jsonl](cases.jsonl)：一行一个案例，`schema_version=1`。
- [run.py](run.py)：仅使用 Python 标准库；默认离线检查。
- [test_runner.py](test_runner.py)：mock 行为测试，不调用真实模型。

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
python eval/run.py
python -m unittest discover -s eval -p 'test_*.py' -v
```

第一条命令检查完整案例集，输出结构检查结果和数据文件 SHA-256；没有网络请求。
第二条命令使用模拟 API 返回值检查脚本行为，也不访问上游模型。
离线检查不会用文本相似度或“数字仍在”推导语义得分。

## 2. 显式调用真实 Babel 服务

先启动 Babel，并在产品中配置需要使用的模型方案。脚本只把显式 `--profile` 名称交给服务，不读取模型配置文件、环境 API Key 或 `/api/config`。
以下方案名只是占位示例，须替换成用户自行选择的已有方案；未知方案由服务拒绝。

联网运行必须同时指定 `--live`、`--endpoint`、`--profile`、`--max-requests` 和新的 `--output` 文件。
`--endpoint` 是 Babel 服务基地址，不是上游模型服务地址。脚本不跟随重定向，不自动重试。
已有输出文件不会覆盖，以免混淆不同运行。

### 生成候选

```bash
python eval/run.py --live compile \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-compiler \
  --limit 2 --max-requests 2 \
  --token-encoding cl100k_base \
  --output eval-runs/compile-pilot.jsonl
```

调用 `POST /api/compile`，对原文生成候选，发送 `verify=false`、`strict_gate=false`、`no_cache=true`。
领域取自案例，输出预设默认 `standard`，可用 `--mode` 修改。拟定候选、反例和预期关系不会发送给编译器。
`--token-encoding` 可选 `estimate`、`cl100k_base`、`o200k_base`；不填写时继承所选产品方案。
选定编码的精确文本计数仍不包含消息封装或工具 schema，也不等于其他模型的 tokenizer 结果。

### 对照已有候选

```bash
python eval/run.py --live validate \
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
python eval/run.py --live validate \
  --endpoint http://127.0.0.1:8080 \
  --profile chosen-comparator \
  --validation-mode blind --reader-profile chosen-reader \
  --case-id zh-null-empty-missing \
  --limit 1 --max-requests 3 \
  --output eval-runs/blind-pilot.jsonl
```

`blind` 请求交由服务执行独立阅读和后续对照；脚本本身不实现或模拟阅读器。
不传 `--reader-profile` 时继承所选方案保存的阅读器设置；传 `--reader-profile chosen-reader` 时覆盖为指定方案。
显式 `--reader-profile=` 会发送空字符串，清除本次请求继承的阅读器覆盖，改用 `--profile` 指定的方案执行阅读；不会修改已保存的配置。
这个参数（包括显式空串）只允许用于 `--live validate --validation-mode blind`。
脚本核对返回的执行方式和协议版本：`blind_read` / `semantic-blind-v1`，以及 `local_checks`、`reader`、`comparator` 三个阶段各自的当前版本。
只有本地检查 `completed`、阅读器 `completed` 且附有 `relations` 非空、`ambiguities=[]` 的结构化阅读回执、对照器 `passed` 时，才把顶层 `passed` 计为独立阅读通过；`entities` 可以为空。
在该模式下收到普通 `compare`、旧协议、缺失阶段或互相矛盾的状态，一律记为 `unknown`，不会降级为通过。
一次验证请求可能涉及阅读与对照两次上游模型调用。`--max-requests` 限制的是 HTTP 请求数，不是上游调用次数、token 或费用上限。

## 3. 选择案例与控制预算

默认只运行文件顺序中的前 **2 个案例**，不是自动运行全量。
`--case-id` 可重复指定；筛选后仍按文件顺序取前 `--limit` 个案例。
编译模式每例最多一个 HTTP 请求；当前验证数据每例三个请求。
计划请求数超过 `--max-requests` 时，执行到预算即停止，余下数量记为 `skipped_by_budget`；不会自动补跑。

输出是逐行写入的 JSONL：运行说明、每次请求观察、最终汇总。
记录含原文、实际候选、预期关系、HTTP 状态、耗时及服务返回的计量元数据；异常的上游响应正文不复制进报告。
`--timeout` 默认每个请求 60 秒。错误会单独记录，不会通过自动重试突破预算。

## 4. 如何阅读结果

`compile` 仅记录生成结果与保护字面量的出现情况，`semantic_quality` 保持 `not_evaluated`。
服务提供的 `measurement_kind/source/scope/encoding/version` 原样保留，脚本不把估算 token 改称实测账单。

`validate` 的 `draft_label_confusion_matrix` 将草拟合法/非法标签与服务 `passed/failed` 结果交叉统计。
普通对照只接受 `semantic-gate-v2` 下的 `llm_compare` 回执，或本地检查明确拒绝的 `local_checks` 回执。
独立阅读允许本地检查拒绝、阅读器拒绝或最终对照拒绝，但后续未运行的阶段必须与拒绝位置一致；阅读器完成本身不算通过。
每条观察记录包含 `requested_mode`、`receipt_check` 和阶段元数据，便于定位协议不匹配。协议升级后须先同步脚本的版本约定，不能仅凭顶层状态继续计分。
`draft_label_agreement` 是这两类已决结果与草拟标签的一致比例，名称和含义均是 **draft-label agreement**，不是事实语义正确率。
`error` 和 `unknown` 单列并排除在一致比例分母之外，不能算作“成功拒绝反例”；没有已决结果时比例为 `null`。
HTTP 502 等非 200 响应始终计为 `error`；HTTP 200 中明确的服务或阶段 `error` 也不计入语义失败。报告保留状态信息，省略异常详情中的上游原始正文。
`api_success` 只表示协议层产生可用结果，不代表候选正确。

运行记录始终标明上游模型身份未独立核验。`--live` 代表启用网络；若 endpoint 指向 mock，它仍然只是 mock 结果。
当前仓库没有随附真实模型分数。正式比较应在人工复核后冻结案例版本，固定模型、验证策略与编码，并保留失败案例；不能边修改案例边报告同一集合上的“提升”。

退出码：`0` 表示结构检查或联网记录正常结束（即使存在草拟标签分歧）；`1` 表示输入、配置或文件错误；`2` 表示联网结果存在 API 异常或未知验证状态。

