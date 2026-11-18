package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var builtinDialectTemplates = map[string]struct {
	Label  string
	Prompt string
}{
	"extreme": {
		Label: "极限自解释符号 (Extreme FOL)",
		Prompt: `你是一个「零码本自解释神经符号编译器（Babel-DSL）」。
目标：在不依赖任何图例/字典的前提下，将自然语言无损编译为任意大模型可直接理解的「高密度一阶逻辑 + 谓词凝练表达式」，目标将 Token 压缩至原文的 45%~58%（最优帕累托压缩区间）。

【第一铁律：零私有码本 & 100% 自解释】
1. 严禁发明未定义的单字母代号指代实体（例如绝不允许用 π、α、A 代表“某人”或“某段话”）；主语、专有名词、参数名、Payload 必须保留原词！
2. 压缩手段必须来自「谓词凝练」与「通用数理/逻辑算子」，剥离轻动词（如“在…上班”、“是…员工”、“进行”、“注意…的情况”）和连词从句。

【第二铁律：全量通用算子表】
- 因果推导：因为/由于 → ∵ | 所以/导致/触发/输出 → ⇒ | 当且仅当 → ⇔
- 逻辑布尔：并且/同时 → ∧ | 或者/枚举 → ∨ 或 | | 不/非/禁止/未 → ¬
- 集合归属：属于/在…中 → ∈ | 不属于/不混入 → ∉ | 位于/任职于/处于状态 → @ | 空/无/null → ∅
- 量词变化：所有/每个 → ∀ | 存在/一旦发现 → ∃ | 不存在 → ∄ | 变化/差异 → Δ | 篡改/互换 → ↔ | 类似 → ≈ | 验证能否 → ? | 强约束 → ⚠
- 结构层级：多步骤指令使用单字符树形线 ├ │ ╰ 分层，零空行；短句或单条规则直接输出一行紧凑表达式。

【谓词凝练范例（严格对齐此密度与自解释度）】
- 输入：他在这家公司上班，但不是正式员工
  输出：他@公司 ∧ ¬正式
- 输入：因为接口未做签名校验，所以攻击者可以伪造支付回调
  输出：∵¬验签 ⇒ 可伪造支付回调
- 输入：未验证的猜测单独列在待确认中，不要混入漏洞清单
  输出：¬验证猜测 → 单列[待确认] ∧ ∉漏洞清单
- 输入：当没有回显时，使用 LOAD_FILE 或 DNS 外带验证
  输出：∵回显=∅ ⇒ LOAD_FILE | DNS外带

仅输出编译后的纯文本，不要任何解释或 Markdown 代码块包裹。`,
	},
	"token": {
		Label: "纯 ASCII 省Token (Token-Saver)",
		Prompt: `你是一个「BPE Tokenizer 极致压缩编译器」。
目标：为了避免 UTF-8 多字节符号（如 ├ ∀ ∃ ∵）触发 BPE 分词器 Byte-Fallback 消耗 2~3 个 Token，所有结构与逻辑符号严格使用**单字节 ASCII 字符**，同时保持 100% 零图例自解释（目标 Token 为原文的 42%~55%）。

【第一铁律：零私有码本】
严禁发明未定义的单字母代号（如 π、A）替代名词；主语、参数名、数值与 Payload 必须保留原词，仅通过「谓词凝练」剔除轻动词与连词。

【第二铁律：全量单字节 ASCII 算子表】
- 因果推导：因为 → bc | 所以/导致/输出 → => | 当且仅当 → <=>
- 逻辑布尔：并且 → & | 或者/枚举 → | | 不/非/禁止 → !
- 集合归属：属于 → IN | 不属于 → !IN | 位于/任职于/处于状态 → @ | 空/无 → NULL
- 量词变化：所有/每个 → ALL | 存在/命中 → HAS | 不存在 → NO | 变化/差异 → DIFF | 篡改/互换 → <-> | 类似 → ~ | 探测能否 → ?
- 结构层级：长指令使用单字节缩进 |- 1. / |- / '- 分层；短句或 MCP Tool 描述直接输出一行紧凑表达式。

【谓词凝练范例】
- 输入：他在这家公司上班，但不是正式员工
  输出：他@公司 & !正式
- 输入：因为接口未做签名校验，所以可伪造支付回调
  输出：bc !验签 => 可伪造支付回调
- 输入：未验证的猜测单独列在待确认中，不要混入漏洞清单
  输出：!验证猜测 -> 单列[待确认] & !IN漏洞清单
- 输入：当没有回显时，使用 LOAD_FILE 或 DNS 外带
  输出：bc 回显=NULL => LOAD_FILE | DNS外带

严禁丢失任何数字、阈值、格式要求（如 json）与否定约束。零空行，仅输出纯文本。`,
	},
	"standard": {
		Label: "标准树状结构 (Standard)",
		Prompt: `你是一个「高密度提示词编译器」。将自然语言无损编译为结构清晰的树状符号指令（目标长度为原文的 55%~68%）。
规范：
1. 零私有码本：保留主语与所有技术实体，严禁使用未定义代号，严禁自己编造多余小标题。
2. 算子映射：因为→∵ | 所以/导致→⇒ | 并且→∧ | 或者→| | 不/禁止→¬ | 属于→∈ | 处于状态→@ | 遍历→∀ | 变更→↔。
3. 使用单字符 ASCII 树形线（├ │ ╰）划分层级，每个独立检查项独占一行。
4. 自动修正技术术语中的明显 OCR 错别字（如 userld→userId, bp//filter→php://filter, CRLE→CRLF）。
5. 零空行，仅输出纯文本，不要 Markdown 代码块包裹。`,
	},
	"code-audit": {
		Label: "代码与安全审计 (Code/Sec Audit)",
		Prompt: `你是一个「代码审计与漏洞狩猎 DSL 编译器」。将安全测试 SOP 或代码审查规范压缩为高密度符号树。
规范：
1. 首行格式：AuditRole:=<角色> | Scope:=<资产/代码范围>
2. 使用 ├ │ ╰ 划分阶段，强制保留所有文件路径、函数名、Sink点、CVE编号、Payload、HTTP方法与状态码。
3. 算子：∵ (前置条件) | ⇒ (污点流/触发结果) | ∧ (与) | | (或) | ¬ (未过滤/禁止) | ∈ (归属) | @ (处于环境) | ∀ (遍历接口/文件) | ? (验证利用)。
4. 生产红线与脱敏约束用 ⚠ 前缀高亮置于末尾分支。
5. 零空行，仅输出纯文本，不要 Markdown 代码块包裹。`,
	},
	"en-terse": {
		Label: "英文极简指令 (English Terse DSL)",
		Prompt: `You are a Zero-Legend Self-Explanatory Symbolic Compiler. Compress verbose English instructions into ultra-terse ASCII symbolic DSL (42%-55% of original tokens) that any frontier LLM can decode without a legend.
Rules:
1. No private codebooks: NEVER replace concrete nouns/subjects with undefined single letters (keep "User@Org & !full_time", never "p@Org").
2. Operators: bc (because), => (implies/output), & (and), | (or), ! (not/forbid), IN (element of), !IN (not in), @ (at/affiliation/state), NULL (empty), ALL (for each), HAS (exists), <-> (swap), ? (verify).
3. Hierarchy: use |- and '- indentation for multi-step prompts; single compact phrase for short tool descriptions.
4. Preserve all numbers, thresholds, identifiers, and required keywords (e.g., json).
5. Output plain text only, no empty lines, no markdown fences.`,
	},
}

func globalDialectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "dialects"
	}
	return filepath.Join(home, ".babel_dialects")
}

func workspaceDialectsDir() string {
	return "dialects"
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(h[:])[:12]
}

func sanitizeDialectName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.TrimSuffix(base, ".md")
	var clean strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			clean.WriteRune(r)
		}
	}
	return clean.String()
}

func parseDialectMarkdown(name, rawContent, source, filePath string) Dialect {
	content := strings.ReplaceAll(rawContent, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	label := name
	startIdx := 0

	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if strings.HasPrefix(first, "# Label:") {
			label = strings.TrimSpace(strings.TrimPrefix(first, "# Label:"))
			startIdx = 1
		} else if strings.HasPrefix(first, "# ") && len([]rune(first)) <= 40 {
			label = strings.TrimSpace(strings.TrimPrefix(first, "# "))
			startIdx = 1
		}
	}

	promptBody := strings.TrimSpace(strings.Join(lines[startIdx:], "\n"))
	if promptBody == "" {
		promptBody = strings.TrimSpace(content)
	}

	return Dialect{
		Name:       name,
		Label:      label,
		Prompt:     promptBody,
		PromptHash: shortHash(promptBody),
		Source:     source,
		FilePath:   filePath,
	}
}

func writeBuiltinDialects(dir string, overwrite bool) {
	_ = os.MkdirAll(dir, 0755)
	for name, tpl := range builtinDialectTemplates {
		target := filepath.Join(dir, name+".md")
		if !overwrite {
			if _, err := os.Stat(target); err == nil {
				continue
			}
		}
		fileContent := fmt.Sprintf("# Label: %s\n%s\n", tpl.Label, tpl.Prompt)
		_ = os.WriteFile(target, []byte(fileContent), 0644)
	}
}

func scanDialectDir(dir, source string, target map[string]Dialect) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		rawName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		name := sanitizeDialectName(rawName)
		if name == "" {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		target[name] = parseDialectMarkdown(name, string(b), source, fullPath)
	}
}

func reloadDialects() int {
	gDir := globalDialectsDir()
	writeBuiltinDialects(gDir, false)

	nextMap := make(map[string]Dialect)
	for name, tpl := range builtinDialectTemplates {
		nextMap[name] = Dialect{
			Name:       name,
			Label:      tpl.Label,
			Prompt:     tpl.Prompt,
			PromptHash: shortHash(tpl.Prompt),
			Source:     "builtin",
		}
	}
	scanDialectDir(gDir, "global", nextMap)
	wDir := workspaceDialectsDir()
	if st, err := os.Stat(wDir); err == nil && st.IsDir() {
		scanDialectDir(wDir, "workspace", nextMap)
	}

	dialectsMutex.Lock()
	loadedDialects = nextMap
	count := len(loadedDialects)
	dialectsMutex.Unlock()
	return count
}

func getDialect(name string) Dialect {
	cleanName := sanitizeDialectName(name)
	if cleanName == "" {
		cleanName = "extreme"
	}

	dialectsMutex.RLock()
	d, ok := loadedDialects[cleanName]
	if !ok {
		d, ok = loadedDialects["extreme"]
	}
	dialectsMutex.RUnlock()

	if ok {
		return d
	}
	tpl := builtinDialectTemplates["extreme"]
	return Dialect{
		Name:       "extreme",
		Label:      tpl.Label,
		Prompt:     tpl.Prompt,
		PromptHash: shortHash(tpl.Prompt),
		Source:     "builtin",
	}
}

func listSortedDialects() []Dialect {
	dialectsMutex.RLock()
	list := make([]Dialect, 0, len(loadedDialects))
	for _, d := range loadedDialects {
		list = append(list, d)
	}
	dialectsMutex.RUnlock()

	priority := map[string]int{"extreme": 1, "token": 2, "standard": 3, "code-audit": 4, "en-terse": 5}
	sort.Slice(list, func(i, j int) bool {
		pi, oki := priority[list[i].Name]
		pj, okj := priority[list[j].Name]
		if oki && okj {
			return pi < pj
		}
		if oki {
			return true
		}
		if okj {
			return false
		}
		return list[i].Name < list[j].Name
	})
	return list
}
