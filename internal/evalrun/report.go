package evalrun

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var reportResultOrder = []string{"api_success", "passed", "failed", "error", "unknown", "not_validated"}

var reportResultLabels = map[string]string{
	"api_success": "生成 API 成功 · 语义未评估", "passed": "所选验证流程通过",
	"failed": "所选验证流程未通过", "error": "执行异常", "unknown": "结果未知", "not_validated": "未执行验证",
}

var reportStageNames = map[string]string{
	"compile": "生成 API", "validate": "验证 API", "local_checks": "本地检查", "reader": "独立阅读器", "comparator": "对照器",
}

var reportStageStatuses = map[string]string{
	"not_run": "未执行", "completed": "已完成", "passed": "通过", "failed": "未通过",
	"error": "异常", "unknown": "未知", "api_success": "API 成功", "not_validated": "未验证",
}

type reportRun struct {
	manifest       Record
	operation      string
	observations   []Record
	summary        Record
	checkedSummary Record
	warnings       []string
	sha256         string
}

// WriteReport renders an existing JSONL run without model calls, configuration
// reads, tokenization or external resources. Existing output is never replaced.
func WriteReport(inputPath, outputPath string) error {
	run, err := loadReport(inputPath)
	if err != nil {
		return err
	}
	rendered := buildReportHTML(run, filepath.Base(inputPath))
	output, err := createOutput(outputPath)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	_, writeErr := output.WriteString(rendered)
	closeErr := output.Close()
	if writeErr != nil {
		return fmt.Errorf("write report: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close report: %w", closeErr)
	}
	return nil
}

func reportPlain(value any) string {
	if value == nil {
		return "未提供"
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "无法表示的记录值"
	}
	return string(encoded)
}

func reportEscape(value any) string { return html.EscapeString(reportPlain(value)) }

func reportNonnegativeInt(value any) bool {
	number, ok := integer(value)
	return ok && number >= 0
}

func reportNumber(value any) (float64, bool) {
	var number float64
	if whole, ok := integer(value); ok {
		number = float64(whole)
	} else {
		switch raw := value.(type) {
		case json.Number:
			var err error
			number, err = raw.Float64()
			if err != nil {
				return 0, false
			}
		case float64:
			number = raw
		default:
			return 0, false
		}
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func reportDuration(value any, milliseconds bool) string {
	number, valid := reportNumber(value)
	if !valid || number < 0 {
		return "未记录"
	}
	if milliseconds {
		return strconv.FormatFloat(number, 'g', -1, 64) + " ms"
	}
	return fmt.Sprintf("%.3f s", number)
}

func reportJSONEqual(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func reportCategory(value any) string {
	category := textValue(value)
	if _, exists := reportResultLabels[category]; exists {
		return category
	}
	return "unknown"
}

func reportWarn(row Record, warning string) {
	warnings, _ := row["_warnings"].([]string)
	row["_warnings"] = append(warnings, warning)
}

func checkReportObservation(row, manifest Record) {
	operation, recorded := textValue(manifest["operation"]), textValue(row["result"])
	category, reason := reportCategory(row["result"]), ""
	allowed := false
	switch operation {
	case "compile":
		allowed = recorded == "api_success" || recorded == "error" || recorded == "unknown"
	case "validate":
		allowed = recorded == "passed" || recorded == "failed" || recorded == "error" || recorded == "unknown"
	case "pipeline":
		allowed = recorded == "passed" || recorded == "failed" || recorded == "error" || recorded == "unknown" || recorded == "not_validated"
	}
	httpStatus, _ := integer(row["http_status"])
	if !allowed {
		category, reason = "unknown", "unexpected_result_for_operation"
	} else if (recorded == "api_success" || recorded == "passed" || recorded == "failed") &&
		(!nonempty(row["source"]) || !nonempty(row["candidate"])) {
		category, reason = "unknown", "missing_source_or_actual_candidate"
	} else if recorded == "api_success" && httpStatus != 200 {
		category, reason = "unknown", "generation_http_status_mismatch"
	} else if recorded == "passed" || recorded == "failed" {
		validation, mode := object(row["validation"]), textValue(manifest["validation_mode"])
		if operation == "pipeline" {
			stage := object(object(row["stages"])["validate"])
			httpStatus, _ = integer(stage["http_status"])
		}
		if (mode != "compare" && mode != "blind") || validation == nil {
			category, reason = "unknown", "missing_requested_mode_or_validation_receipt"
		} else {
			recognized, detail := ValidationObservation(int(httpStatus), Record{"validation": validation}, mode)
			if recognized != recorded {
				category, reason = "unknown", textValue(detail["receipt_check"])
			} else if validation["requested_mode"] != mode {
				category, reason = "unknown", "recorded_requested_mode_mismatch"
			} else if !reportJSONEqual(validation["receipt_check"], detail["receipt_check"]) {
				category, reason = "unknown", "recorded_receipt_check_mismatch"
			}
		}
	}
	row["_display_result"] = category
	if reason != "" {
		reportWarn(row, fmt.Sprintf("原始 result=%s 无法认可：%s。展示为 unknown，并排除验证通过率分母。", reportPlain(row["result"]), reason))
	}
	if operation == "pipeline" {
		measurement, selection := PipelineMeasurement(row["stats"], category, nonempty(row["candidate"]))
		row["_display_measurement"], row["_display_selection"] = measurement, selection
		for _, entry := range []struct {
			key     string
			checked Record
		}{{"measurement", measurement}, {"selection", selection}} {
			original := object(row[entry.key])
			matches := original != nil
			for field, value := range entry.checked {
				if !reportJSONEqual(original[field], value) {
					matches = false
				}
			}
			if !matches {
				reportWarn(row, "原始 "+entry.key+" 与同一评测规则的复核结果不一致；正文展示复核结果，原记录保留在明细中。")
			}
		}
	}
}

func checkedReportSummary(summary Record, observations []Record, operation string) Record {
	if summary == nil {
		return nil
	}
	counts := map[string]int64{}
	for _, row := range observations {
		counts[textValue(row["_display_result"])]++
	}
	checked := Record{"requests": summary["requests"], "planned_requests": summary["planned_requests"],
		"skipped_by_budget": summary["skipped_by_budget"], "error": counts["error"],
		"unknown": counts["unknown"], "not_validated": counts["not_validated"]}
	if operation == "pipeline" {
		decided := counts["passed"] + counts["failed"]
		var rate any
		if decided > 0 {
			rate = float64(counts["passed"]) / float64(decided)
		}
		groups, excluded := map[string]Record{}, map[string]int{}
		selectionCounts := Record{"candidate": int64(0), "source": int64(0)}
		for _, row := range observations {
			selection := object(row["_display_selection"])
			addPipelineMeasurement(groups, excluded, object(row["_display_measurement"]), selection)
			output := textValue(selection["output"])
			count, _ := integer(selectionCounts[output])
			selectionCounts[output] = count + 1
		}
		checked["validation_decided"], checked["validation_pass_rate"] = decided, rate
		checked["excluded_from_validation"] = Record{"error": counts["error"], "unknown": counts["unknown"], "not_validated": counts["not_validated"]}
		checked["measurement_groups"], checked["measurement_excluded"] = sortedMeasurementGroups(groups), excluded
		checked["selection_counts"] = selectionCounts
	} else if operation == "validate" {
		matrix := Record{"draft_valid": Record{"passed": int64(0), "failed": int64(0)}, "draft_invalid": Record{"passed": int64(0), "failed": int64(0)}}
		var missing, decided, agreed int64
		for _, row := range observations {
			result := textValue(row["_display_result"])
			if result != "passed" && result != "failed" {
				continue
			}
			expected := textValue(row["draft_expected_validation"])
			if expected != "passed" && expected != "failed" {
				missing++
				continue
			}
			label := "draft_invalid"
			if expected == "passed" {
				label = "draft_valid"
			}
			values := object(matrix[label])
			count, _ := integer(values[result])
			values[result] = count + 1
			decided++
			if result == expected {
				agreed++
			}
		}
		var rate any
		if decided > 0 {
			rate = float64(agreed) / float64(decided)
		}
		checked["draft_label_confusion_matrix"], checked["draft_label_agreement"] = matrix, rate
		checked["draft_label_agreement_denominator"] = decided
		checked["excluded_from_agreement"] = Record{"error": counts["error"], "unknown": counts["unknown"], "missing_draft_label": missing}
	}
	return checked
}

func loadReport(path string) (*reportRun, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read report input: %w", err)
	}
	run := &reportRun{observations: []Record{}, warnings: []string{}, sha256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	seen := map[[2]string]bool{}
	for index, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		number := index + 1
		record, err := DecodeRecord(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: invalid JSON: %w", filepath.Base(path), number, err)
		}
		if run.manifest == nil {
			if record["record_type"] != "manifest" {
				return nil, fmt.Errorf("line %d: expected a run manifest, not a case dataset or raw API response", number)
			}
			operation := textValue(record["operation"])
			version, valid := integer(record["format_version"])
			if !valid || !((operation == "compile" || operation == "validate") && version == 1 || operation == "pipeline" && version == 2) {
				return nil, fmt.Errorf("unsupported run format: operation=%s, format_version=%s", reportPlain(record["operation"]), reportPlain(record["format_version"]))
			}
			run.manifest, run.operation = record, operation
			continue
		}
		if run.summary != nil {
			return nil, fmt.Errorf("line %d: record appears after the final summary", number)
		}
		switch record["record_type"] {
		case "summary":
			if record["operation"] != run.operation {
				return nil, fmt.Errorf("line %d: summary operation differs from manifest", number)
			}
			run.summary = record
		case "observation":
			if operation, exists := record["operation"]; exists && operation != run.operation {
				return nil, fmt.Errorf("line %d: observation operation differs from manifest", number)
			}
			if !nonempty(record["case_id"]) || !nonempty(record["candidate_id"]) {
				return nil, fmt.Errorf("line %d: observation needs nonempty case_id and candidate_id", number)
			}
			record["_line"], record["_warnings"] = number, []string{}
			if _, ok := record["source"].(string); !ok {
				reportWarn(record, "原文 source 缺失或不是字符串。")
			}
			if _, ok := record["candidate"].(string); !ok {
				result := textValue(record["result"])
				if result == "passed" || result == "failed" || result == "api_success" {
					reportWarn(record, "结果声称已生成或完成验证，但实际候选 candidate 缺失。")
				} else if record["candidate"] != nil {
					reportWarn(record, "candidate 不是字符串。")
				}
			}
			if _, known := reportResultLabels[textValue(record["result"])]; !known {
				reportWarn(record, "未知或缺失的 result；保留原始值，按未知结果筛选。")
			}
			key := [2]string{textValue(record["case_id"]), textValue(record["candidate_id"])}
			if seen[key] {
				reportWarn(record, "案例与候选标识重复；报告保留全部记录。")
			}
			seen[key] = true
			checkReportObservation(record, run.manifest)
			run.observations = append(run.observations, record)
		default:
			return nil, fmt.Errorf("line %d: unsupported record_type %s", number, reportPlain(record["record_type"]))
		}
	}
	if run.manifest == nil {
		return nil, fmt.Errorf("input is empty; no run manifest or observations")
	}
	if run.summary == nil {
		run.warnings = append(run.warnings, "缺少最终 summary：运行可能被中断。本页只展示已经写入的观察，不补算运行汇总。")
	} else {
		key := "requests"
		if run.operation == "pipeline" {
			key = "cases"
		}
		number, valid := integer(run.summary[key])
		if !valid || number < 0 || number != int64(len(run.observations)) {
			run.warnings = append(run.warnings, "summary."+key+" 与实际观察行数不一致；汇总可能不完整或已被修改。")
		}
	}
	if _, ok := run.manifest["annotation_status"].(string); !ok {
		run.warnings = append(run.warnings, "manifest 未提供 annotation_status，不能确认案例已经人工复核。")
	}
	for _, row := range run.observations {
		if len(row["_warnings"].([]string)) > 0 {
			run.warnings = append(run.warnings, "部分观察记录不完整或包含未知字段值，请查看对应案例的警示。")
			break
		}
	}
	run.checkedSummary = checkedReportSummary(run.summary, run.observations, run.operation)
	var changed []string
	for key, value := range run.checkedSummary {
		if original, exists := run.summary[key]; exists && !reportJSONEqual(original, value) {
			changed = append(changed, key)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		run.warnings = append(run.warnings, "原始汇总与复核后的观察不一致："+strings.Join(changed, ", ")+"。正文以观察复核为准，原始汇总保留在明细中。")
	}
	return run, nil
}

func reportFields(items [][2]any) string {
	var output strings.Builder
	output.WriteString("<dl>")
	for _, item := range items {
		output.WriteString("<dt>" + reportEscape(item[0]) + "</dt><dd>" + reportEscape(item[1]) + "</dd>")
	}
	output.WriteString("</dl>")
	return output.String()
}

func reportRecordFields(record Record, items [][2]string) string {
	values := make([][2]any, 0, len(items))
	for _, item := range items {
		values = append(values, [2]any{item[0], record[item[1]]})
	}
	return reportFields(values)
}

func reportJSONDetails(label string, value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return reportWarningList([]string{label + "：记录无法显示为 JSON。"})
	}
	// JSON stays escaped DOM text: no run data enters scripts or attributes.
	return "<details><summary>" + reportEscape(label) + "</summary><pre><code>" + reportEscape(string(encoded)) + "</code></pre></details>"
}

func reportWarningList(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString(`<div class="warning"><ul>`)
	for _, warning := range warnings {
		output.WriteString("<li>" + reportEscape(warning) + "</li>")
	}
	output.WriteString("</ul></div>")
	return output.String()
}

func reportMeasurementLabel(meta Record) string {
	if meta == nil {
		return "计量口径未提供"
	}
	complete := true
	for _, key := range measurementFields {
		complete = complete && nonempty(meta[key])
	}
	if !complete || meta["measurement_scope"] != "text" {
		return "计量口径不完整或范围不是 text，不能确认文本计数"
	}
	switch meta["measurement_kind"] {
	case "estimated":
		return "估算文本 Token（记录口径）"
	case "exact":
		if strings.EqualFold(strings.TrimSpace(textValue(meta["measurement_encoding"])), "estimate") ||
			strings.EqualFold(strings.TrimSpace(textValue(meta["measurement_source"])), "heuristic-v2") {
			return "计量声明矛盾，不能确认精确文本计数"
		}
		return "指定编码精确文本 Token（服务记录声明）"
	default:
		return "未识别的计量口径，保留原始记录"
	}
}

func reportMeasurementMeta(meta Record) string {
	return reportRecordFields(meta, [][2]string{
		{"计量方式", "measurement_kind"}, {"来源", "measurement_source"}, {"范围", "measurement_scope"},
		{"编码", "measurement_encoding"}, {"版本", "measurement_version"},
	})
}

func renderReportMeasurement(row Record) string {
	measurement, stats := object(row["_display_measurement"]), row["stats"]
	if measurement == nil {
		measurement = object(row["measurement"])
	}
	var meta Record
	var original, candidate any
	var output strings.Builder
	if measurement != nil {
		if measurement["status"] != "included" {
			output.WriteString(reportRecordFields(measurement, [][2]string{{"计量汇总状态", "status"}, {"未纳入原因", "reason"}}))
			if stats != nil {
				output.WriteString(reportJSONDetails("服务返回的计量记录（未纳入汇总）", stats))
			}
			return output.String()
		}
		meta, original, candidate = object(measurement["group"]), measurement["original_tokens"], measurement["candidate_tokens"]
	} else {
		meta = object(stats)
		if meta == nil {
			return `<p class="muted">本记录未提供 Token 计量；报告不根据文本长度补算。</p>`
		}
		original, candidate = meta["original_tokens"], meta["compiled_tokens"]
	}
	output.WriteString("<p><strong>" + reportEscape(reportMeasurementLabel(meta)) + "</strong></p>" + reportMeasurementMeta(meta))
	if reportNonnegativeInt(original) && reportNonnegativeInt(candidate) {
		output.WriteString(`<p class="token-count">原文 ` + reportEscape(original) + " → 候选 " + reportEscape(candidate) + " tok</p>")
	} else {
		output.WriteString(reportWarningList([]string{"Token 数值缺失或无效，未显示数值比较。"}))
	}
	if measurement != nil && reportNonnegativeInt(measurement["effective_tokens"]) {
		output.WriteString("<p>按规则采用/回退后的文本：" + reportEscape(measurement["effective_tokens"]) + " tok（相同记录口径，未发送下游）</p>")
	}
	if stats != nil {
		output.WriteString(reportJSONDetails("服务返回的计量元数据", stats))
	}
	return output.String()
}

func reportLookupLabel(labels map[string]string, value any) string {
	if label, exists := labels[textValue(value)]; exists {
		return label
	}
	return reportPlain(value)
}

func renderReportStage(name string, value any, httpStage bool) string {
	stage := object(value)
	if stage == nil {
		return reportWarningList([]string{name + " 阶段记录无效。"})
	}
	status := stage["status"]
	if httpStage {
		status = stage["result"]
	}
	items := [][2]any{{"状态", reportLookupLabel(reportStageStatuses, status)}}
	if httpStage {
		items = append(items, [2]any{"HTTP", stage["http_status"]}, [2]any{"耗时", reportDuration(stage["elapsed_seconds"], false)})
	} else {
		items = append(items, [2]any{"模型（记录值）", stage["model"]}, [2]any{"政策版本", stage["policy_version"]},
			[2]any{"耗时", reportDuration(stage["duration_ms"], true)})
	}
	if stage["skip_reason"] != nil {
		items = append(items, [2]any{"跳过原因", stage["skip_reason"]})
	}
	var output strings.Builder
	output.WriteString(`<section class="stage"><h4>` + reportEscape(reportLookupLabel(reportStageNames, name)) + "</h4>" + reportFields(items))
	if len(array(stage["issues"])) > 0 || nonempty(stage["issues"]) {
		output.WriteString(reportJSONDetails("阶段问题", stage["issues"]))
	}
	if stage["issues_omitted"] == true {
		output.WriteString(`<p class="note">运行器已省略阶段异常详情；报告没有补回上游原始响应。</p>`)
	}
	if reading, exists := stage["reading"]; exists {
		output.WriteString(reportJSONDetails("阅读器结构化理解 · reading", reading))
	}
	output.WriteString("</section>")
	return output.String()
}

func renderReportValidation(row Record) string {
	var output strings.Builder
	if stages := object(row["stages"]); stages != nil {
		output.WriteString("<h3>请求阶段</h3>")
		var extra []string
		for key := range stages {
			if key != "compile" && key != "validate" {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		for _, name := range append([]string{"compile", "validate"}, extra...) {
			if stage, exists := stages[name]; exists {
				output.WriteString(renderReportStage(name, stage, true))
			}
		}
	}
	validation := object(row["validation"])
	if validation == nil {
		return output.String() + `<p class="muted">未提供验证回执。</p>`
	}
	output.WriteString("<h3>验证回执</h3>" + reportRecordFields(validation, [][2]string{
		{"请求模式", "requested_mode"}, {"回执检查", "receipt_check"}, {"记录状态", "status"},
		{"执行方法", "method"}, {"政策版本", "policy_version"},
	}))
	if issues := array(validation["issues"]); issues != nil {
		output.WriteString("<h4>记录中的问题</h4>")
		if len(issues) == 0 {
			output.WriteString(`<p class="muted">问题列表为空；这不证明语义完整。</p>`)
		} else {
			output.WriteString("<ul>")
			for _, issue := range issues {
				output.WriteString("<li>" + reportEscape(issue) + "</li>")
			}
			output.WriteString("</ul>")
		}
	}
	if validation["issues_omitted"] == true {
		output.WriteString(`<p class="note">运行器已省略异常详情；状态仍保留为异常或未知。</p>`)
	}
	if stages := array(validation["stages"]); stages != nil {
		for _, raw := range stages {
			if stage := object(raw); stage != nil {
				name := "未知阶段"
				if value, exists := stage["name"]; exists {
					name = reportPlain(value)
				}
				output.WriteString(renderReportStage(name, stage, false))
			} else {
				output.WriteString(reportWarningList([]string{"验证阶段记录不是对象。"}))
			}
		}
	} else if validation["stages"] != nil {
		output.WriteString(reportWarningList([]string{"validation.stages 不是数组，未尝试推断阶段结果。"}))
	}
	return output.String()
}

func renderReportCase(row Record, number int, operation string) string {
	category := reportCategory(row["_display_result"])
	var output strings.Builder
	fmt.Fprintf(&output, `<article class="case" data-case="%d" data-result="%s">`, number, category)
	output.WriteString(`<div class="case-head"><h2>` + reportEscape(row["case_id"]) + " / " + reportEscape(row["candidate_id"]) +
		`</h2><span class="badge result-` + category + `">` + reportEscape(reportResultLabels[category]) + "</span></div>")
	warnings, _ := row["_warnings"].([]string)
	output.WriteString(reportWarningList(warnings))
	output.WriteString(reportFields([][2]any{{"原始结果字段", row["result"]}, {"JSONL 行号", row["_line"]},
		{"HTTP", row["http_status"]}, {"观察耗时", reportDuration(row["elapsed_seconds"], false)}}))
	selection := object(row["_display_selection"])
	if selection == nil {
		selection = object(row["selection"])
	}
	if selection != nil {
		choice := "未识别的输出选择"
		if selection["output"] == "candidate" {
			choice = "候选"
		} else if selection["output"] == "source" {
			choice = "回退原文"
		}
		output.WriteString(reportFields([][2]any{{"按评测规则选择（未发送下游）", choice}, {"选择原因", selection["reason"]}}))
	}
	if operation == "pipeline" && (!reportJSONEqual(row["selection"], row["_display_selection"]) || !reportJSONEqual(row["measurement"], row["_display_measurement"])) {
		output.WriteString(reportJSONDetails("原始计量与输出选择记录（正文使用复核结果）", Record{"measurement": row["measurement"], "selection": row["selection"]}))
	}
	candidateLabel := "本次生成的实际候选"
	if operation == "validate" {
		candidateLabel = "提交验证的既有候选"
	}
	output.WriteString(`<div class="columns">`)
	for _, item := range [][2]string{{"source", "原文"}, {"candidate", candidateLabel}} {
		output.WriteString("<section><h3>" + item[1] + "</h3>")
		if text, ok := row[item[0]].(string); ok {
			// The code child preserves the initial newline that bare pre drops.
			output.WriteString(`<pre><code data-text-role="` + item[0] + `">` + reportEscape(text) + "</code></pre>")
		} else {
			output.WriteString(`<p class="note">文本未生成或未记录。</p>`)
		}
		output.WriteString("</section>")
	}
	output.WriteString("</div><h3>计数依据</h3>" + renderReportMeasurement(row) + renderReportValidation(row))
	if expected, exists := row["draft_expected_validation"]; exists {
		output.WriteString(reportFields([][2]any{{"草拟标签的期望结果", expected}}))
	}
	for _, item := range [][2]string{
		{"literal_presence_check", "保护字面量出现检查 · 不代表语义保留"}, {"expected_relations", "案例中的关系参考 · 请结合标注状态审阅"},
		{"protected_literals", "保护字面量"}, {"usage", "服务记录的 usage · 不推导账单节省"},
	} {
		if value, exists := row[item[0]]; exists {
			output.WriteString(reportJSONDetails(item[1], value))
		}
	}
	output.WriteString("</article>")
	return output.String()
}

func renderReportSummary(summary Record, operation string, observationCount int) string {
	if summary == nil {
		return `<p class="note">未提供最终汇总。</p>`
	}
	var output strings.Builder
	output.WriteString(reportRecordFields(summary, [][2]string{
		{"记录的 HTTP 请求数", "requests"}, {"计划请求上限", "planned_requests"}, {"因预算未执行", "skipped_by_budget"},
		{"API 异常", "error"}, {"未知结果", "unknown"},
	}))
	rateKey, denominatorKey, label := "draft_label_agreement", "draft_label_agreement_denominator", "草拟标签一致比例"
	if operation == "pipeline" {
		rateKey, denominatorKey, label = "validation_pass_rate", "validation_decided", "所选验证流程通过率"
	}
	if value, exists := summary[rateKey]; exists {
		rate, valid := reportNumber(value)
		denominator, whole := integer(summary[denominatorKey])
		display := "未建立（没有有效的已决分母或比例）"
		if observationCount > 0 && valid && rate >= 0 && rate <= 1 && whole && denominator > 0 {
			display = fmt.Sprintf("%.1f%%（记录分母 %d）", rate*100, denominator)
		}
		output.WriteString(reportFields([][2]any{{label, display}}))
		output.WriteString(`<p class="note">此比例来自运行记录，不能作为语义准确率、下游行为成功率或费用指标。草拟标签也不等于独立真值。</p>`)
	}
	for _, item := range [][2]string{
		{"excluded_from_validation", "排除在流程通过率之外的记录"}, {"excluded_from_agreement", "排除在标签一致比例之外的记录"},
		{"draft_label_confusion_matrix", "草拟标签与流程结果交叉记录"}, {"measurement_excluded", "未纳入计数汇总的原因"},
	} {
		if value, exists := summary[item[0]]; exists {
			output.WriteString(reportJSONDetails(item[1], value))
		}
	}
	if selection := object(summary["selection_counts"]); selection != nil {
		output.WriteString(reportRecordFields(selection, [][2]string{{"按规则采用候选数（未发送下游）", "candidate"}, {"按规则回退原文数", "source"}}))
	}
	if groups := array(summary["measurement_groups"]); groups != nil {
		output.WriteString(`<h3>文本计数分组（按观察复核）</h3><p class="muted">不同编码、来源、版本及估算/精确口径分别展示，不合并 Token 总量。采用/回退是评测规则推导，未发送下游。</p>`)
		for _, raw := range groups {
			group := object(raw)
			if group == nil {
				output.WriteString(reportWarningList([]string{"计量分组不是对象，未展示为有效分组。"}))
				continue
			}
			output.WriteString(`<section class="measurement-group"><h4>` + reportEscape(reportMeasurementLabel(group)) + "</h4>" + reportMeasurementMeta(group))
			output.WriteString(reportRecordFields(group, [][2]string{
				{"案例数", "cases"}, {"原文文本 Token", "original_tokens"}, {"候选文本 Token", "candidate_tokens"},
				{"按规则采用/回退后的文本 Token", "effective_tokens"}, {"候选 Token 变化（正数增加）", "candidate_delta_tokens"},
				{"按规则回退后减少的文本 Token", "effective_savings_tokens"}, {"按规则采用候选数", "adopted_candidates"}, {"按规则回退原文数", "fallback_to_source"},
			}))
			if changes := object(group["candidate_changes"]); changes != nil {
				output.WriteString(reportRecordFields(changes, [][2]string{{"候选缩小案例", "reduced"}, {"候选不变案例", "unchanged"}, {"候选变大案例", "expanded"}}))
			}
			output.WriteString("</section>")
		}
	} else if summary["measurement_groups"] != nil {
		output.WriteString(reportWarningList([]string{"measurement_groups 不是数组，未汇总文本计数。"}))
	}
	return output.String()
}

func buildReportHTML(run *reportRun, inputName string) string {
	manifest := run.manifest
	annotation := manifest["annotation_status"]
	annotationLabel := reportPlain(annotation)
	if annotation == "assistant_drafted_requires_human_review" {
		annotationLabel = "助手草拟，待人工复核"
	}
	readerLabel := manifest["reader_profile"]
	if readerLabel == nil {
		readerLabel = "继承所选方案"
	} else if readerLabel == "" {
		readerLabel = "与验证方案相同"
	}
	validationProfile, exists := manifest["validation_profile"]
	if !exists {
		validationProfile = manifest["profile"]
	}
	encoding := manifest["token_encoding_requested"]
	if encoding == nil || encoding == "" {
		encoding = "继承所选方案"
	}
	identity, exists := manifest["upstream_model_identity"]
	if !exists {
		identity = "未独立核验"
	}
	state := "缺少最终汇总 · 运行记录不完整"
	if run.summary != nil {
		state = "已记录最终汇总"
		if len(run.warnings) > 0 {
			state += " · 存在记录警示"
		}
	}
	selectionScope := "未评估下游选择"
	if run.operation == "pipeline" {
		selectionScope = "仅评测规则，未发送下游"
	}
	var content strings.Builder
	content.WriteString("<header><h1>Babel 离线评测报告</h1><p>" + reportEscape(inputName) + "</p>")
	content.WriteString(`<p class="muted">本页使用运行器的同一套规则复核已有回执和计量记录，不调用模型、不重新分词、不复核语义。校验通过只表示所选流程的记录结果。</p>`)
	content.WriteString(reportFields([][2]any{
		{"运行记录状态", state}, {"操作", run.operation}, {"案例标注状态", annotationLabel}, {"标注状态原值", annotation},
		{"开始时间（记录值）", manifest["started_at_utc"]}, {"编译/主要方案", manifest["profile"]}, {"验证方案", validationProfile},
		{"阅读器方案", readerLabel}, {"验证模式", manifest["validation_mode"]}, {"输出规则（记录值）", manifest["output_preset"]},
		{"请求编码", encoding}, {"输出选择范围", selectionScope}, {"数据集 SHA-256", manifest["dataset_sha256"]}, {"上游模型身份", identity},
	}))
	content.WriteString(reportWarningList(run.warnings) + "</header>")
	content.WriteString(`<section class="overview"><h2>观察复核汇总</h2><p class="muted">通过率与计数分组来自复核后的观察；原始 summary 不覆盖观察结果。HTTP 请求/预算数量沿用运行记录。</p>`)
	content.WriteString(renderReportSummary(run.checkedSummary, run.operation, len(run.observations)))
	if run.summary != nil {
		original := Record{}
		for key, value := range run.summary {
			if key != "endpoint" {
				original[key] = value
			}
		}
		content.WriteString(reportJSONDetails("原始运行器汇总（保留供核查）", original))
	}
	content.WriteString("</section>")
	counts := map[string]int{}
	for _, row := range run.observations {
		counts[reportCategory(row["_display_result"])]++
	}
	content.WriteString(`<section class="overview"><h2>观察记录</h2><p>以下数量按本文件中的观察行计数，不代表 HTTP 请求数或上游模型调用数。</p>`)
	var items [][2]any
	for _, key := range reportResultOrder {
		if count := counts[key]; count > 0 {
			items = append(items, [2]any{reportResultLabels[key], count})
		}
	}
	content.WriteString(reportFields(items))
	content.WriteString(`<div class="filters"><label>结果 <select id="result-filter"><option value="all">全部</option>`)
	for _, key := range reportResultOrder {
		content.WriteString(`<option value="` + key + `">` + reportEscape(reportResultLabels[key]) + "</option>")
	}
	content.WriteString(`</select></label><label>搜索 <input id="search" type="search" placeholder="案例、文本、问题或模型" autocomplete="off"></label>`)
	fmt.Fprintf(&content, `<span id="visible-count" role="status">显示 %d / %d 条观察</span></div>`, len(run.observations), len(run.observations))
	content.WriteString("<noscript><p>JavaScript 未启用，当前展示全部记录；筛选和搜索不可用。</p></noscript></section>")
	if len(run.observations) == 0 {
		content.WriteString(`<section class="overview" id="empty-results"><h2>没有观察记录</h2><p>本文件没有实际候选或验证结果。未生成通过率、语义分数或文本计数。</p></section>`)
	}
	content.WriteString(`<p id="no-matches" class="note" hidden>没有符合当前筛选条件的记录。</p>`)
	for index, row := range run.observations {
		content.WriteString(renderReportCase(row, index+1, run.operation))
	}
	content.WriteString("<footer>输入文件 SHA-256：<code>" + run.sha256 + "</code><br>所有内容来自输入记录；运行配置的 endpoint 未展示。报告无外部资源。</footer>")
	scriptHash := sha256.Sum256([]byte(reportScript))
	policy := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(scriptHash[:]) + "'; style-src 'unsafe-inline'; connect-src 'none'; base-uri 'none'; form-action 'none'"
	return "<!doctype html>\n" + `<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<meta http-equiv="Content-Security-Policy" content="` + reportEscape(policy) + `"><title>Babel 评测报告 · ` + reportEscape(inputName) +
		"</title><style>" + reportCSS + "</style></head><body>" + content.String() + "<script>" + reportScript + "</script></body></html>\n"
}

const reportCSS = `:root{font-family:system-ui,-apple-system,'Segoe UI',sans-serif;color:#172033;background:#f4f6f8;line-height:1.55}
body{max-width:1180px;margin:0 auto;padding:24px}h1{margin:0 0 8px}h2{font-size:1.2rem}h3{font-size:1rem}
header,section.overview,article.case{background:white;border:1px solid #dce2e9;border-radius:8px;padding:20px;margin:16px 0}
.muted{color:#596579}.note,.warning{padding:12px;border-left:4px solid #b98527;background:#fff8e9;overflow-wrap:anywhere}
.warning{border-color:#b24545;background:#fff1f1}dl{display:grid;grid-template-columns:minmax(130px,auto) 1fr;gap:5px 18px;margin:12px 0}
dt{color:#596579}dd{margin:0;overflow-wrap:anywhere}.filters{display:flex;flex-wrap:wrap;gap:12px;align-items:center}
input,select{font:inherit;border:1px solid #b9c3d0;border-radius:4px;padding:7px;max-width:100%}input{min-width:200px}
.case-head{display:flex;flex-wrap:wrap;justify-content:space-between;gap:10px}.badge{border-radius:4px;background:#eef1f6;padding:3px 8px;font-size:.9rem}
.result-passed{background:#e7f0fb}.result-failed,.result-error{background:#fbe8e8}.columns{display:grid;grid-template-columns:1fr 1fr;gap:16px}
.columns>section{min-width:0}pre{background:#f7f8fa;border:1px solid #e1e5eb;padding:12px;margin:8px 0;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;tab-size:4}
code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace}table{border-collapse:collapse;width:100%;margin:10px 0}th,td{border:1px solid #dce2e9;padding:7px;text-align:left;vertical-align:top;overflow-wrap:anywhere}
details{margin:10px 0}summary{cursor:pointer;color:#255e92}.measurement-group{border-top:1px solid #dce2e9;padding-top:12px;margin-top:14px}
.stage{border-left:2px solid #dce2e9;padding-left:12px;margin:12px 0}[hidden]{display:none!important}footer{color:#596579;margin:24px 0;overflow-wrap:anywhere}
@media(max-width:700px){body{padding:12px}.columns{grid-template-columns:1fr}dl{grid-template-columns:1fr;gap:2px}dd{margin-bottom:6px}}`

// Only constant JavaScript is emitted. Search reads escaped DOM text and never
// inserts records into HTML, executable source, event handlers or URLs.
const reportScript = `(() => {
  const cards = Array.from(document.querySelectorAll('[data-case]'));
  const search = document.getElementById('search');
  const result = document.getElementById('result-filter');
  const count = document.getElementById('visible-count');
  const noMatches = document.getElementById('no-matches');
  function applyFilters() {
    const query = search.value.trim().toLocaleLowerCase();
    let visible = 0;
    cards.forEach(card => {
      const matches = (result.value === 'all' || card.dataset.result === result.value) &&
        (!query || card.textContent.toLocaleLowerCase().includes(query));
      card.hidden = !matches;
      if (matches) visible++;
    });
    count.textContent = '显示 ' + visible + ' / ' + cards.length + ' 条观察';
    noMatches.hidden = visible !== 0 || cards.length === 0;
  }
  search.addEventListener('input', applyFilters);
  result.addEventListener('change', applyFilters);
  applyFilters();
})();`
