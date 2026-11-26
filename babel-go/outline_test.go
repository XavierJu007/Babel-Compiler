package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInheritedOutlineTitlesRecognizesCopiedHeadings(t *testing.T) {
	const chinese = "一、受理资格判定\n1. 根据状态判定。\n二、金额分级与审批链\n1. 检查金额。\n三、执行流程\n1. 执行操作。\n四、异常与并发处理\n1. 处理异常。\n五、记录与质检\n1. 保留记录。\n六、职责边界\n1. 遵守边界。"
	const copied = "一、受理资格判定\n状态符合才受理。\n二、金额分级与审批链\n检查金额后审批。\n三、执行流程\n依次执行。\n四、异常与并发处理\n处理异常。\n五、记录与质检\n留痕。\n六、职责边界\n遵守。"
	// An action-like prefix such as 记录 is conservatively left unclassified.
	// The remaining unambiguous titles still establish the copied outline.
	want := []string{"受理资格判定", "金额分级与审批链", "执行流程", "异常与并发处理", "职责边界"}
	if got := inheritedOutlineTitles(chinese, copied); !reflect.DeepEqual(got, want) {
		t.Fatalf("copied Chinese outline: got %v; want %v", got, want)
	}
	const english = "1. Eligibility\nAn eligibility rule.\n2. Approval chain\nAn approval rule."
	if got := inheritedOutlineTitles(english, "1. Eligibility\nShort rule.\n2. Approval chain\nShort rule."); !reflect.DeepEqual(got, []string{"Eligibility", "Approval chain"}) {
		t.Fatalf("copied English outline not recognized: %v", got)
	}
	if got := inheritedOutlineTitles(english, "1. Eligibility\nShort rule."); got != nil {
		t.Fatalf("one coincidentally retained title is not an inherited outline: %v", got)
	}
	for _, candidate := range []string{
		"资格: 状态∈可受理状态\n审批: 金额→对应审批人\n顺序: 校验→执行→留痕",
		"退款\n├许可: 状态∈可受理状态\n╰顺序: 金额→审批人→执行→留痕",
		"受理资格判定→金额分级与审批链→执行流程",
	} {
		if got := inheritedOutlineTitles(chinese, candidate); got != nil {
			t.Errorf("compact relation/tree incorrectly treated as a copied outline: %q: %v", candidate, got)
		}
	}
	for _, candidate := range []string{
		"├1 受理资格判定\n│ 状态∈可受理状态\n╰2 金额分级与审批链\n  金额→审批人",
		"一、受理资格判定: 状态∈可受理状态\n二、金额分级与审批链: 金额→审批人",
	} {
		if got := inheritedOutlineTitles(chinese, candidate); len(got) < 2 {
			t.Errorf("decorating copied sections bypassed restructuring: %q: %v", candidate, got)
		}
	}
}

func TestInheritedOutlineTitlesRecognizesFormattingOnlyChanges(t *testing.T) {
	const source = "1. Eligibility\nAn eligibility rule.\n2. Approval chain\nAn approval rule."
	want := []string{"Eligibility", "Approval chain"}
	for name, candidate := range map[string]string{
		"no numbering":        "Eligibility\nShort rule.\nApproval chain\nShort rule.",
		"trailing colons":     "Eligibility:\nShort rule.\nApproval chain：\nShort rule.",
		"Markdown headings":   "## Eligibility\nShort rule.\n## Approval chain\nShort rule.",
		"Markdown emphasis":   "**Eligibility:**\nShort rule.\n**Approval chain**:\nShort rule.",
		"cosmetic tree":       "├─ Eligibility:\n│ role: reviewer\n╰─ Approval chain:\n  amount→approver",
		"shallow indentation": "  Eligibility:\n  role: reviewer\n  Approval chain:\n  amount→approver",
		"inline relations":    "Eligibility: state∈eligible\nApproval chain: amount→approver",
	} {
		t.Run(name, func(t *testing.T) {
			if got := inheritedOutlineTitles(source, candidate); !reflect.DeepEqual(got, want) {
				t.Fatalf("formatting hid the copied sections: got %v; want %v", got, want)
			}
		})
	}
	const escapedSource = "1\\. Eligibility\nAn eligibility rule.\n2\\. Approval chain\nAn approval rule."
	if got := inheritedOutlineTitles(escapedSource, "Eligibility:\nShort rule.\nApproval chain:\nShort rule."); !reflect.DeepEqual(got, want) {
		t.Fatalf("Markdown-escaped source headings were not recognized: %v", got)
	}
	for name, candidate := range map[string]string{
		"code":       "```text\nEligibility:\nApproval chain:\n```",
		"quotation":  "「\nEligibility:\nApproval chain:\n」",
		"blockquote": "> Eligibility:\n> Approval chain:",
		"indented":   "    Eligibility:\n    Approval chain:",
		"one title":  "Eligibility:\nThe approval path is attached to each eligibility branch.",
	} {
		t.Run(name, func(t *testing.T) {
			if got := inheritedOutlineTitles(source, candidate); got != nil {
				t.Fatalf("protected or noninherited title was rejected: %v", got)
			}
		})
	}
}

func TestReportedRefundOutlineStillNeedsRestructuring(t *testing.T) {
	source, reorganized := refundSOPFixture(t)
	reported, err := os.ReadFile(filepath.Join("..", "docs", "examples", "refund-sop.reported.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := inheritedOutlineTitles(source, string(reported)); len(got) < 2 {
		t.Fatalf("reported six-section rewrite escaped shape checks: %v", got)
	}
	if got := inheritedOutlineTitles(source, reorganized); got != nil {
		t.Fatalf("reorganized relations were mistaken for copied sections: %v", got)
	}
}

func TestBabelStructureIssuesRequiresTreeForExpandedSections(t *testing.T) {
	const source = "1. Eligibility\nAn eligibility rule.\n2. Approval chain\nAn approval rule."
	const treeIssue = "多分支正文未组织为树：需至少两个带正文的树分支（├ / └ / ╰）"
	const copiedIssue = "候选仍复刻原章节标题: Eligibility、Approval chain"
	for name, candidate := range map[string]string{
		"renamed flat relations":   "Inputs: state=ready\nSequence: amount→approver",
		"renamed plain paragraphs": "Inputs\nAn input rule.\nSequence\nAn approval rule.",
		"single candidate chain":   "eligibility→approval→execution",
		"only one branch":          "Task\n├state=ready\napprover: supervisor",
		"empty decoration":         "Task\n├──\n└──",
		"mentioned characters":     "Use markers ├ and └ for branches.",
		"fenced branches":          "```text\n├state=ready\n└amount→approver\n```",
		"quoted branches":          "「\n├state=ready\n└amount→approver\n」",
		"blockquote branches":      "> ├state=ready\n> └amount→approver",
		"one unquoted branch":      "├state=ready\n`└amount→approver`",
		"candidate format claim":   "Output format: flat\nInputs: state=ready\nSequence: amount→approver",
	} {
		t.Run(name, func(t *testing.T) {
			if got := babelStructureIssues(source, candidate); !reflect.DeepEqual(got, []string{treeIssue}) {
				t.Fatalf("flat candidate bypassed the tree contract: %v", got)
			}
		})
	}
	if got := babelStructureIssues(source, "Eligibility:\nShort rule.\nApproval chain:\nShort rule."); !reflect.DeepEqual(got, []string{copiedIssue, treeIssue}) {
		t.Fatalf("copied flat chapters did not report both structure problems: %v", got)
	}
	if got := babelStructureIssues(source, "├Eligibility\n│ state=ready\n└Approval chain\n  amount→approver"); !reflect.DeepEqual(got, []string{copiedIssue}) {
		t.Fatalf("tree decoration hid the original chapters: %v", got)
	}
	if got := babelStructureIssues(source, "Output format: tree\n├Eligibility\n│ state=ready\n└Approval chain\n  amount→approver"); !reflect.DeepEqual(got, []string{copiedIssue}) {
		t.Fatalf("candidate invented a format exemption for copied chapters: %v", got)
	}
	for _, candidate := range []string{
		"Task\n├state=ready→may proceed\n└amount→approver",
		"Task\n├conditions\n│ ╰state=ready\n╰amount→approver",
		"States\n├「READY」\n╰「DONE」",
	} {
		if got := babelStructureIssues(source, candidate); got != nil {
			t.Fatalf("visible reorganized tree rejected: %v", got)
		}
	}
}

func TestBabelStructureIssuesKeepsSourceExemptions(t *testing.T) {
	const expanded = "1. Eligibility\nAn eligibility rule.\n2. Approval chain\nAn approval rule."
	for name, original := range map[string]string{
		"single relation":     "An approved request may execute.",
		"single section":      "1. Eligibility\nAn eligibility rule.",
		"compact sections":    "1. Eligibility\nstate=ready\n2. Approval chain\namount→approver",
		"explicit format":     "Output format:\n" + expanded,
		"referenced sections": expanded + "\nSee section 2.",
		"source code":         "```text\n" + expanded + "\n```",
		"source quotation":    "「\n" + expanded + "\n」",
	} {
		t.Run(name, func(t *testing.T) {
			if got := babelStructureIssues(original, "Inputs: state=ready\nSequence: amount→approver"); got != nil {
				t.Fatalf("tree requirement overrode an exempt source: %v", got)
			}
		})
	}
}

func TestOutlineRecognizerProtectsLiteralsReferencesAndOutputContracts(t *testing.T) {
	const outline = "1. Eligibility\n2. Approval chain"
	for name, text := range map[string]string{
		"fenced code":            "```text\n" + outline + "\n```",
		"unclosed code fence":    "```text\n" + outline,
		"tilde code fence":       "~~~text\n" + outline + "\n~~~",
		"double quoted block":    "\"" + outline + "\"",
		"Chinese quoted block":   "「\n" + outline + "\n」",
		"indented code":          "    1. Eligibility\n    2. Approval chain",
		"tab indented code":      "\t1. Eligibility\n\t2. Approval chain",
		"English reference":      outline + "\nRetry according to section 2.",
		"Chinese reference":      outline + "\n失败时返回第 2 步。",
		"Chinese step reference": outline + "\n步骤2决定下一动作。",
		"Chinese bracket ref":    outline + "\n失败时参见（2）。",
		"English bracket ref":    outline + "\nOn failure, see (2).",
		"preserve numbering":     "必须保留原文编号。\n" + outline,
		"English numbering":      "Preserve the original numbering.\n" + outline,
		"output template":        "输出格式：\n" + outline,
		"English template":       "Output format:\n" + outline,
		"explicit format":        "按照以下格式输出：\n" + outline,
	} {
		t.Run(name, func(t *testing.T) {
			if got := inheritedOutlineTitles(text, text); got != nil {
				t.Fatalf("protected layout incorrectly rejected: %v", got)
			}
			if got := structuralNumberSpans(text); got != nil {
				t.Fatalf("protected numbers would be discarded: %v", got)
			}
		})
	}
	// Explicitly required source formatting remains exempt even when a model
	// omits the requirement's prose from the candidate.
	if got := inheritedOutlineTitles("输出格式：\n"+outline, outline); got != nil {
		t.Fatalf("source output requirement lost its protection: %v", got)
	}
	// A closed indented fence must not turn the following prose into code.
	withFence := "  ```text\n1. literal\n2. literal\n  ```\n" + outline
	if got := structuralNumberSpans(withFence); len(got) != 2 {
		t.Fatalf("closed fence swallowed later layout: %v", got)
	}
}

func TestStructuralNumberSpansOnlyCoverSequentialLayoutLabels(t *testing.T) {
	const text = "1. Retain logs for 180 days.\n2. Retry at most 3 times.\n3. Wait 5 seconds."
	var want [][2]int
	for _, prefix := range []string{"1. Retain", "2. Retry", "3. Wait"} {
		start := strings.Index(text, prefix)
		want = append(want, [2]int{start, start + 1})
	}
	if got := structuralNumberSpans(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("layout spans=%v; want %v", got, want)
	}
	const repeated = "一、资格\n1. 校验金额 99 元。\n2. 保留记录 180 天。\n二、审批\n1. 提交凭证。\n2. 等待审批。"
	if got := structuralNumberSpans(repeated); len(got) != 4 {
		t.Fatalf("independent sequential lists not recognized: %v", got)
	}
	const closing = "1) 检查金额 500 元\n2) 处理时限 48 小时"
	if got := structuralNumberSpans(closing); len(got) != 2 {
		t.Fatalf("parenthesized labels not recognized: %v", got)
	}
	const headings = "# 1. Eligibility\n# 2. Approval chain"
	if got := structuralNumberSpans(headings); len(got) != 2 {
		t.Fatalf("Markdown numbered headings not recognized: %v", got)
	}
	const escaped = "1\\. Retain logs for 180 days.\n2\\. Retry at most 3 times."
	if got := structuralNumberSpans(escaped); len(got) != 2 {
		t.Fatalf("Markdown-escaped sequential labels not recognized: %v", got)
	}
	var businessValues []string
	for _, number := range numberOccurrences(escaped) {
		businessValues = append(businessValues, number.Value)
	}
	if !reflect.DeepEqual(businessValues, []string{"180", "3"}) {
		t.Fatalf("escaped list labels became business numbers or body values disappeared: %v", businessValues)
	}
}

func TestOutlineRecognitionLeavesBusinessNumbersAndCompactInputsAlone(t *testing.T) {
	for _, text := range []string{
		"99 元至 500 元：需主管审批\n500 元至 2000 元：需双签",
		"1.25 元\n2.50 元",
		"1. 元\n2. 元",
		"1. 99 元\n2. 500 元",
		"2026-11-20\n2026-11-21",
		"1. November\n2. November",
		"09:00 生成日报\n24:00 结束",
		"v1.2.3\nv2.3.4",
		"1.2.3\n2.3.4",
		"1.3 版本\n2.4 版本",
		"0.alpha\n0.beta",
		"1. A single heading",
		"7. Eligibility\n8. Approval chain",
		"├1 资格: 金额≤99元\n╰2 审批: 金额>99元",
	} {
		t.Run(text, func(t *testing.T) {
			if got := structuralNumberSpans(text); got != nil {
				t.Fatalf("ambiguous/business numbers incorrectly exempted: %v", got)
			}
		})
	}
	for _, text := range []string{
		"1. Check the logs\n2. Save the result",
		"一、检查日志\n二、保存结果",
		"一、时限: ≤48小时\n二、条件: 金额≤实付",
		"1. logs→archive\n2. amount≥0",
		"一、资格\n状态∈可受理集合\n二、审批\n金额≤99元→可直办",
		"1. Eligibility\nrole: reviewer\nstate=ready\n2. Approval chain\namount<99→approval optional",
	} {
		if got := inheritedOutlineTitles(text, text); got != nil {
			t.Fatalf("compact steps/relations mistaken for heading-only outline: %q: %v", text, got)
		}
	}
}

func TestStandaloneArticleHeadingsDoNotProtectSequentialLists(t *testing.T) {
	const zhContract = "第一条 主体资质\n1. 提交营业执照。\n2. 成立满 180 天。\n第二条 结算周期\n1. 延迟超过 15 天。\n2. 预付款不超过 20%。"
	if got := structuralNumberSpans(zhContract); len(got) != 4 {
		t.Fatalf("standalone article headings falsely protected sequential list labels: %v", got)
	}
	var values []string
	for _, n := range numberOccurrences(zhContract) {
		values = append(values, n.Value)
	}
	if !reflect.DeepEqual(values, []string{"180", "15", "20%"}) {
		t.Fatalf("unexpected business numbers for zhContract: %v", values)
	}

	const enSections = "Section 1: Auth\n1. Require HMAC.\n2. Reject skew > 300s."
	if got := structuralNumberSpans(enSections); len(got) != 2 {
		t.Fatalf("standalone Section heading falsely protected sequential list labels: %v", got)
	}

	const withInlineRef = zhContract + "\n违反第一条规定须承担责任。"
	if got := structuralNumberSpans(withInlineRef); got != nil {
		t.Fatalf("inline article reference lost protection: %v", got)
	}
}
