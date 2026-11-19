package main

import (
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
		"├1 受理资格判定\n│ 状态∈可受理状态\n╰2 金额分级与审批链\n  金额→审批人",
		"一、受理资格判定: 状态∈可受理状态\n二、金额分级与审批链: 金额→审批人",
	} {
		if got := inheritedOutlineTitles(chinese, candidate); got != nil {
			t.Errorf("compact relation/tree incorrectly treated as a copied outline: %q: %v", candidate, got)
		}
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
