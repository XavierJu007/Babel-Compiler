package evalrun

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Use the standard library's permissive XML tokenizer to inspect the emitted
// HTML tree. Tests verify decoded DOM text and attributes, not only substrings.
type reportTestNode struct {
	tag      string
	attrs    map[string]string
	children []any
}

func (node *reportTestNode) text() string {
	var output strings.Builder
	for _, child := range node.children {
		switch value := child.(type) {
		case string:
			output.WriteString(value)
		case *reportTestNode:
			output.WriteString(value.text())
		}
	}
	return output.String()
}

func (node *reportTestNode) find(tag string, attrs map[string]string) []*reportTestNode {
	matches := tag == "" || node.tag == tag
	for key, value := range attrs {
		if node.attrs[key] != value {
			matches = false
		}
	}
	var result []*reportTestNode
	if matches {
		result = append(result, node)
	}
	for _, child := range node.children {
		if nested, ok := child.(*reportTestNode); ok {
			result = append(result, nested.find(tag, attrs)...)
		}
	}
	return result
}

func reportTestDOM(t *testing.T, markup string) *reportTestNode {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(markup))
	decoder.Strict, decoder.AutoClose, decoder.Entity = false, xml.HTMLAutoClose, xml.HTMLEntity
	root := &reportTestNode{tag: "document", attrs: map[string]string{}}
	stack := []*reportTestNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return root
		}
		if err != nil {
			t.Fatalf("parse generated HTML: %v", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			node := &reportTestNode{tag: value.Name.Local, attrs: map[string]string{}}
			for _, attr := range value.Attr {
				node.attrs[attr.Name.Local] = attr.Value
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.EndElement:
			for index := len(stack) - 1; index > 0; index-- {
				if stack[index].tag == value.Name.Local {
					stack = stack[:index]
					break
				}
			}
		case xml.CharData:
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, string(value))
		}
	}
}

func reportTestStats(encoding, kind, source string, original, candidate int64) Record {
	version := "v0.3.0"
	if kind == "estimated" {
		version = "2"
	}
	return Record{"original_tokens": original, "compiled_tokens": candidate,
		"measurement_kind": kind, "measurement_encoding": encoding, "measurement_source": source,
		"measurement_scope": "text", "measurement_version": version}
}

func reportTestReceipt(status string, blind bool) Record {
	mode := "compare"
	body := compareReceiptFixture(status)
	if blind {
		mode, body = "blind", blindReceiptFixture()
		if status == "failed" {
			body = blindRejectionFixture("comparator")
		}
		for index, value := range array(object(body["validation"])["stages"]) {
			object(value)["duration_ms"] = []int{1, 12, 19}[index]
		}
	}
	httpStatus := 200
	if status == "error" {
		httpStatus = 502
		validation := object(body["validation"])
		validation["status"], validation["issues"] = "error", []any{"fixture error"}
		if blind {
			reader := receiptStage(body, "reader")
			reader["status"], reader["issues"] = "error", []any{"reader error"}
			delete(reader, "reading")
			receiptStage(body, "comparator")["status"] = "not_run"
			receiptStage(body, "comparator")["duration_ms"] = nil
		}
	}
	_, detail := ValidationObservation(httpStatus, body, mode)
	return detail
}

func reportTestObservation(operation, caseID, result string, counts Record) Record {
	if counts == nil {
		counts = reportTestStats("cl100k_base", "exact", "tiktoken-go/tokenizer", 30, 20)
	}
	row := Record{"record_type": "observation", "operation": operation, "case_id": caseID, "candidate_id": "generated",
		"source": "  source  text\nnext line\n", "candidate": " actual  API candidate\n",
		"result": result, "http_status": 200, "elapsed_seconds": json.Number("0.25"), "stats": counts,
		"expected_relations": []any{Record{"id": "r1", "statement": "Keep the condition."}}, "protected_literals": []any{"  "}}
	if operation != "compile" {
		row["validation"] = reportTestReceipt(result, operation == "pipeline")
	}
	if operation == "validate" {
		row["draft_expected_validation"] = "passed"
	}
	if operation == "pipeline" {
		status := 200
		if result == "error" {
			status = 502
		}
		row["stages"] = Record{
			"compile":  Record{"result": "api_success", "http_status": 200, "elapsed_seconds": json.Number("0.1")},
			"validate": Record{"result": result, "http_status": status, "elapsed_seconds": json.Number("0.15")},
		}
		row["measurement"], row["selection"] = PipelineMeasurement(counts, result, true)
	}
	return row
}

func reportTestRecords(operation string, rows []Record, final bool, updates Record) []Record {
	format, planned, mode := 1, len(rows), "compare"
	if operation == "pipeline" {
		format, planned, mode = 2, 2*len(rows), "blind"
	}
	manifest := Record{"record_type": "manifest", "format_version": format, "operation": operation,
		"annotation_status": "assistant_drafted_requires_human_review", "profile": "compiler-fixture",
		"validation_profile": "comparator-fixture", "validation_mode": mode, "reader_profile": nil,
		"dataset_sha256": strings.Repeat("a", 64), "endpoint": "https://internal-private-endpoint.example/v1",
		"started_at_utc": "2026-01-01T00:00:00+00:00", "planned_requests": planned, "selection_scope": "evaluation_rule_only"}
	records := append([]Record{manifest}, rows...)
	if final {
		summary := Record{"record_type": "summary", "operation": operation, "requests": planned,
			"cases": len(rows), "planned_requests": planned, "skipped_by_budget": 0, "error": 0, "unknown": 0}
		for _, row := range rows {
			if row["result"] == "error" {
				increment(summary, "error", 1)
			}
		}
		for key, value := range updates {
			summary[key] = value
		}
		records = append(records, summary)
	}
	return records
}

func reportTestInput(t *testing.T, records []Record, filename string) string {
	t.Helper()
	var data bytes.Buffer
	for _, record := range records {
		if err := writeRecord(&data, record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reportTestRender(t *testing.T, records []Record, name string) (*reportRun, string, *reportTestNode) {
	t.Helper()
	path := reportTestInput(t, records, name)
	run, err := loadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	markup := buildReportHTML(run, name)
	return run, markup, reportTestDOM(t, markup)
}

func reportTestOne(t *testing.T, node *reportTestNode, tag string, attrs map[string]string) *reportTestNode {
	t.Helper()
	matches := node.find(tag, attrs)
	if len(matches) != 1 {
		t.Fatalf("expected one %s %v, got %d", tag, attrs, len(matches))
	}
	return matches[0]
}

func TestReportUntrustedContentIsDOMTextAndCSPRemainsConstant(t *testing.T) {
	attack := "\n  </code></pre></script><script>alert(\"x\")</script><img src=x onerror=\"boom\"> & ' \"\u2028\u2029  \n"
	row := reportTestObservation("pipeline", attack, "passed", nil)
	row["source"], row["candidate"] = attack, attack
	row["expected_relations"] = []any{Record{"id": attack, "statement": attack}}
	row["protected_literals"] = []any{attack}
	validation := object(row["validation"])
	validation["issues"] = []any{attack}
	reader := object(array(validation["stages"])[1])
	reader["model"] = attack
	object(reader["reading"])["entities"] = []any{attack}
	counts := object(row["stats"])
	counts["measurement_source"], counts["measurement_version"] = attack, attack
	records := reportTestRecords("pipeline", []Record{row}, true, Record{"endpoint": "private-summary-endpoint"})
	records[0]["profile"], records[0]["annotation_status"] = attack, attack
	_, markup, dom := reportTestRender(t, records, "run-\"<&.jsonl")
	if reportTestOne(t, dom, "script", nil).text() != reportScript || reportTestOne(t, dom, "style", nil).text() != reportCSS {
		t.Fatal("untrusted input changed executable script or CSS")
	}
	for _, tag := range []string{"img", "iframe"} {
		if len(dom.find(tag, nil)) != 0 {
			t.Fatalf("injected %s element", tag)
		}
	}
	for _, node := range dom.find("", nil) {
		for key := range node.attrs {
			if strings.HasPrefix(strings.ToLower(key), "on") || key == "src" || key == "href" || key == "srcdoc" {
				t.Fatalf("unexpected executable or external attribute %q", key)
			}
		}
	}
	for _, role := range []string{"source", "candidate"} {
		if got := reportTestOne(t, dom, "code", map[string]string{"data-text-role": role}).text(); got != attack {
			t.Fatalf("%s text changed: %q", role, got)
		}
	}
	policy := reportTestOne(t, dom, "meta", map[string]string{"http-equiv": "Content-Security-Policy"}).attrs["content"]
	digest := sha256.Sum256([]byte(reportScript))
	if !strings.Contains(policy, "connect-src 'none'") || !strings.Contains(policy, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'") {
		t.Fatal("CSP does not restrict the emitted script and network access")
	}
	if strings.Contains(markup, "private-endpoint") || strings.Contains(markup, "private-summary-endpoint") {
		t.Fatal("configuration endpoint leaked into report")
	}
}

func TestReportPreservesActualTextAndWhitespaceInBothV1Modes(t *testing.T) {
	for operation, result := range map[string]string{"compile": "api_success", "validate": "failed"} {
		t.Run(operation, func(t *testing.T) {
			row := reportTestObservation(operation, "case", result, nil)
			row["source"], row["candidate"] = "\n\n  before  \t after\n", "\n actual   returned candidate\n  "
			row["valid_candidate"] = "NEVER DISPLAY THIS FIXTURE SUBSTITUTE"
			_, markup, dom := reportTestRender(t, reportTestRecords(operation, []Record{row}, true, nil), "run.jsonl")
			for _, role := range []string{"source", "candidate"} {
				if reportTestOne(t, dom, "code", map[string]string{"data-text-role": role}).text() != row[role] {
					t.Fatalf("%s text was normalized or substituted", role)
				}
			}
			if strings.Contains(markup, "NEVER DISPLAY") {
				t.Fatal("a drafted candidate substituted the actual candidate")
			}
		})
	}
}

func TestReportEmptyAndInterruptedRunsDoNotInventFinalMetrics(t *testing.T) {
	run, _, dom := reportTestRender(t, reportTestRecords("validate", nil, true, nil), "empty.jsonl")
	if len(run.warnings) > 0 || len(dom.find("article", nil)) != 0 {
		t.Fatalf("valid empty run: warnings=%v", run.warnings)
	}
	reportTestOne(t, dom, "section", map[string]string{"id": "empty-results"})
	row := reportTestObservation("pipeline", "case", "error", nil)
	run, _, dom = reportTestRender(t, reportTestRecords("pipeline", []Record{row}, false, nil), "partial.jsonl")
	if run.summary != nil || run.checkedSummary != nil || !strings.Contains(strings.Join(run.warnings, " "), "缺少最终 summary") {
		t.Fatal("interrupted run acquired a manufactured final summary")
	}
	if len(dom.find("article", nil)) != 1 || strings.Contains(dom.text(), "100.0%") {
		t.Fatal("partial observations or their unknown metrics were misrepresented")
	}
}

func TestReportStageErrorsAndFoldedReadingStayDistinct(t *testing.T) {
	row := reportTestObservation("pipeline", "case", "error", nil)
	_, _, dom := reportTestRender(t, reportTestRecords("pipeline", []Record{row}, true, nil), "errors.jsonl")
	card := reportTestOne(t, dom, "article", nil)
	if card.attrs["data-result"] != "error" {
		t.Fatal("stage failure became a pass")
	}
	stages := card.find("section", map[string]string{"class": "stage"})
	if len(stages) != 5 {
		t.Fatalf("expected two HTTP and three validation stages, got %d", len(stages))
	}
	for _, stage := range stages {
		name := reportTestOne(t, stage, "h4", nil).text()
		if name == "独立阅读器" && (!strings.Contains(stage.text(), "异常") || !strings.Contains(stage.text(), "12 ms") || !strings.Contains(stage.text(), "运行器已省略阶段异常详情")) {
			t.Fatalf("reader failure lost its evidence: %s", stage.text())
		}
		if name == "对照器" && !strings.Contains(stage.text(), "未执行") {
			t.Fatal("comparator incorrectly shown as completed")
		}
	}
	for _, details := range card.find("details", nil) {
		if strings.Contains(reportTestOne(t, details, "summary", nil).text(), "reading") {
			t.Fatal("missing reader output was invented")
		}
	}
	_, _, dom = reportTestRender(t, reportTestRecords("pipeline", []Record{reportTestObservation("pipeline", "good", "passed", nil)}, true, nil), "passed.jsonl")
	found := false
	for _, details := range dom.find("details", nil) {
		if !strings.Contains(reportTestOne(t, details, "summary", nil).text(), "reading") {
			continue
		}
		found = true
		if _, open := details.attrs["open"]; open {
			t.Fatal("large reader output is expanded by default")
		}
		reading, err := DecodeRecord([]byte(reportTestOne(t, details, "code", nil).text()))
		if err != nil || !reflect.DeepEqual(array(reading["entities"]), []any{"cache"}) {
			t.Fatalf("reader JSON cannot be recovered: %v %v", reading, err)
		}
	}
	if !found || !strings.Contains(dom.text(), "按评测规则选择（未发送下游）") {
		t.Fatal("reading or evaluation-only selection scope was omitted")
	}
}

func TestReportSeparatesCountingBasesAndExcludesContradictoryMetadata(t *testing.T) {
	variants := []Record{
		reportTestStats("estimate", "estimated", "heuristic-v2", 300, 100),
		reportTestStats("cl100k_base", "exact", "tiktoken-go/tokenizer", 40, 20),
		reportTestStats("o200k_base", "exact", "tiktoken-go/tokenizer", 20, 10),
		reportTestStats("estimate", "exact", "heuristic-v2", 30, 20),
	}
	var rows []Record
	for index, counts := range variants {
		rows = append(rows, reportTestObservation("pipeline", string(rune('a'+index)), "passed", counts))
	}
	run, _, dom := reportTestRender(t, reportTestRecords("pipeline", rows, true, nil), "mixed.jsonl")
	groups := dom.find("section", map[string]string{"class": "measurement-group"})
	if len(groups) != 3 || len(dom.find("p", map[string]string{"class": "token-count"})) != 3 {
		t.Fatal("incompatible measurement bases were merged or an excluded count was displayed")
	}
	for index, encoding := range []string{"estimate", "cl100k_base", "o200k_base"} {
		if !strings.Contains(groups[index].text(), encoding) {
			t.Fatalf("group %d lost its encoding", index)
		}
	}
	if !strings.Contains(dom.text(), "原文 300 → 候选 100 tok") {
		t.Fatal("service counts were replaced with a text-length heuristic")
	}
	last := dom.find("article", nil)[3]
	if len(last.find("p", map[string]string{"class": "token-count"})) != 0 || !strings.Contains(last.text(), "inconsistent_measurement_metadata") {
		t.Fatal("contradictory exact claim displayed as a confirmed count")
	}
	if !strings.HasPrefix(reportMeasurementLabel(variants[3]), "计量声明矛盾") {
		t.Fatal("exact/estimate contradiction was not named")
	}
	if len(array(run.checkedSummary["measurement_groups"])) != 3 || strings.Contains(dom.text(), "实际账单节省") {
		t.Fatal("measurement scope changed")
	}
}

func TestReportKeepsRecognizedDenominatorAndRejectsForgedSummary(t *testing.T) {
	rows := []Record{
		reportTestObservation("pipeline", "a", "passed", nil), reportTestObservation("pipeline", "b", "failed", nil),
		reportTestObservation("pipeline", "c", "error", nil),
	}
	_, _, dom := reportTestRender(t, reportTestRecords("pipeline", rows, true, Record{
		"validation_pass_rate": json.Number("0.5"), "validation_decided": 2,
	}), "rates.jsonl")
	if !strings.Contains(dom.text(), "50.0%（记录分母 2）") {
		t.Fatal("error entered the decided denominator")
	}
	for _, heading := range dom.find("dt", nil) {
		if heading.text() == "语义准确率" {
			t.Fatal("validation flow rate relabelled as semantic accuracy")
		}
	}

	for _, failure := range []string{"wrong_method", "wrong_requested_mode", "wrong_receipt_check", "missing_stage"} {
		t.Run(failure, func(t *testing.T) {
			row := reportTestObservation("pipeline", "case", "passed", nil)
			receipt := object(row["validation"])
			switch failure {
			case "wrong_method":
				receipt["method"], receipt["policy_version"], receipt["receipt_check"] = "llm_compare", "semantic-gate-v2", "blind_method_or_policy_mismatch"
			case "wrong_requested_mode":
				receipt["requested_mode"] = "compare"
			case "wrong_receipt_check":
				receipt["receipt_check"] = "blind_method_or_policy_mismatch"
			case "missing_stage":
				receipt["stages"] = array(receipt["stages"])[1:]
			}
			run, _, root := reportTestRender(t, reportTestRecords("pipeline", []Record{row}, true,
				Record{"validation_pass_rate": 1, "validation_decided": 1}), "contradiction.jsonl")
			if run.observations[0]["result"] != "passed" || run.observations[0]["_display_result"] != "unknown" {
				t.Fatal("raw evidence lost or contradictory positive result recognized")
			}
			denominator, _ := integer(run.checkedSummary["validation_decided"])
			if denominator != 0 || run.checkedSummary["validation_pass_rate"] != nil {
				t.Fatal("summary overrode the recognized observations")
			}
			selection := object(run.checkedSummary["selection_counts"])
			candidates, _ := integer(selection["candidate"])
			sources, _ := integer(selection["source"])
			if candidates != 0 || sources != 1 {
				t.Fatal("unrecognized validation still adopted a candidate")
			}
			group := object(array(run.checkedSummary["measurement_groups"])[0])
			if !reportJSONEqual(group["effective_tokens"], object(row["stats"])["original_tokens"]) {
				t.Fatal("rule fallback did not restore source count")
			}
			if reportTestOne(t, root, "article", nil).attrs["data-result"] != "unknown" || strings.Contains(root.text(), "100.0%") {
				t.Fatal("contradictory receipt still rendered as passed")
			}
			if !strings.Contains(strings.Join(run.warnings, " "), "原始汇总与复核后的观察不一致") {
				t.Fatal("raw summary disagreement has no warning")
			}
		})
	}
}

func TestReportUnknownResultAndMissingActualTextRemainVisible(t *testing.T) {
	row := reportTestObservation("compile", "case", "api_success", nil)
	row["result"], row["source"], row["candidate"] = Record{"unexpected": "value"}, nil, nil
	run, _, dom := reportTestRender(t, reportTestRecords("compile", []Record{row}, true, nil), "unknown.jsonl")
	if len(run.warnings) == 0 || reportTestOne(t, dom, "article", nil).attrs["data-result"] != "unknown" {
		t.Fatal("unknown observation not identified")
	}
	for _, role := range []string{"source", "candidate"} {
		if len(dom.find("code", map[string]string{"data-text-role": role})) != 0 {
			t.Fatal("missing text was fabricated")
		}
	}
	for _, invalidCount := range []any{true, json.Number("30.0"), "30", int64(-1)} {
		row = reportTestObservation("pipeline", "bad-count", "passed", nil)
		object(row["stats"])["original_tokens"] = invalidCount
		run, _, dom = reportTestRender(t, reportTestRecords("pipeline", []Record{row}, true, nil), "counts.jsonl")
		if object(run.observations[0]["_display_measurement"])["reason"] != "invalid_token_counts" ||
			len(dom.find("p", map[string]string{"class": "token-count"})) != 0 {
			t.Fatalf("invalid count %v was presented as measured", invalidCount)
		}
	}
}

func TestReportMalformedFilesFailBeforeCreatingOutput(t *testing.T) {
	manifest := `{"record_type":"manifest","operation":"compile","format_version":1}`
	samples := []string{"", "{}\n", "[]\n", "null\n", "\xff\n",
		`{"record_type":"manifest","record_type":"summary"}`, manifest + ` {}`,
		`{"record_type":"manifest","operation":"compile","format_version":99}`,
		`{"record_type":"manifest","operation":"compile","format_version":1.0}`,
		`{"record_type":"manifest","operation":"compile","format_version":true}`,
		`{"record_type":"manifest","operation":"compile","format_version":1,"x":NaN}`,
		`{"record_type":"manifest","operation":"compile","format_version":1,"x":{"a":1,"a":2}}`,
		manifest + "\n" + `{"record_type":`,
		manifest + "\n" + `{"record_type":"summary","operation":"validate"}`,
		manifest + "\n" + `{"record_type":"summary","operation":"compile"}` + "\n" + `{"record_type":"observation"}`,
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "bad.jsonl"), filepath.Join(dir, "out.html")
	for _, sample := range samples {
		if err := os.WriteFile(input, []byte(sample), 0600); err != nil {
			t.Fatal(err)
		}
		if err := WriteReport(input, output); err == nil {
			t.Fatalf("invalid run accepted: %q", sample)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("invalid input created output: %v", err)
		}
	}
}

func TestReportWriteAndCLIRefuseExistingOutputAndInputPath(t *testing.T) {
	input := reportTestInput(t, reportTestRecords("compile", []Record{reportTestObservation("compile", "case", "api_success", nil)}, true, nil), "run.jsonl")
	source, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "nested", "report.html")
	if err := WriteReport(input, output); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(output)
	if err != nil || len(reportTestDOM(t, string(saved)).find("article", nil)) != 1 {
		t.Fatalf("report missing: %v", err)
	}
	for _, target := range []string{output, input} {
		if err := WriteReport(input, target); err == nil {
			t.Fatalf("existing path overwritten: %s", target)
		}
	}
	unchanged, _ := os.ReadFile(output)
	original, _ := os.ReadFile(input)
	if !bytes.Equal(saved, unchanged) || !bytes.Equal(source, original) {
		t.Fatal("an existing file changed")
	}
	var stdout, stderr bytes.Buffer
	cliOutput := filepath.Join(t.TempDir(), "cli.html")
	if status := RunCLI([]string{"report", input, "-output", cliOutput}, &stdout, &stderr); status != 0 {
		t.Fatalf("Go report CLI failed (%d): %s", status, stderr.String())
	}
	markup, err := os.ReadFile(cliOutput)
	if err != nil || len(reportTestDOM(t, string(markup)).find("article", nil)) != 1 {
		t.Fatalf("CLI report not written: %v", err)
	}
}

func TestReportShippedSearchAndFilterChangeVisibleCards(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("optional Node runtime unavailable for frontend JavaScript behavior test")
	}
	rows := []Record{reportTestObservation("validate", "alpha", "passed", nil), reportTestObservation("validate", "beta", "error", nil),
		reportTestObservation("validate", "gamma", "unknown", nil)}
	rows[0]["source"], rows[1]["source"] = "needle FOR Alpha", "needle FOR Beta"
	_, _, dom := reportTestRender(t, reportTestRecords("validate", rows, true, nil), "filter.jsonl")
	var cards []any
	for _, card := range dom.find("article", nil) {
		cards = append(cards, Record{"result": card.attrs["data-result"], "text": card.text()})
	}
	payload, err := json.Marshal(Record{"script": reportTestOne(t, dom, "script", nil).text(), "cards": cards,
		"queries": []any{Record{"result": "all", "search": "NEEDLE"}, Record{"result": "error", "search": "needle"},
			Record{"result": "passed", "search": "Beta"}, Record{"result": "all", "search": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	js := `const fs=require('fs'), vm=require('vm');
const data=JSON.parse(fs.readFileSync(0,'utf8'));
const cards=data.cards.map(card=>({dataset:{result:card.result},textContent:card.text,hidden:false}));
const controls={};
for(const id of ['search','result-filter','visible-count','no-matches'])
  controls[id]={value:id==='result-filter'?'all':'',handlers:{},addEventListener(name,fn){this.handlers[name]=fn;}};
vm.runInNewContext(data.script,{document:{querySelectorAll:()=>cards,getElementById:id=>controls[id]}});
const states=[];
for(const query of data.queries){
  controls.search.value=query.search;controls['result-filter'].value=query.result;
  controls.search.handlers.input();controls['result-filter'].handlers.change();
  states.push({visible:cards.map((card,i)=>card.hidden?null:i).filter(i=>i!==null),
    message:controls['visible-count'].textContent,noMatchesHidden:controls['no-matches'].hidden});
}
process.stdout.write(JSON.stringify(states));`
	command := exec.Command(node, "-e", js)
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("frontend filter failed: %v %s", err, output)
	}
	var states []struct {
		Visible         []int  `json:"visible"`
		Message         string `json:"message"`
		NoMatchesHidden bool   `json:"noMatchesHidden"`
	}
	if err := json.Unmarshal(output, &states); err != nil {
		t.Fatal(err)
	}
	want := [][]int{{0, 1}, {1}, {}, {0, 1, 2}}
	if len(states) != len(want) {
		t.Fatalf("filter states: %s", output)
	}
	for index, state := range states {
		if !reflect.DeepEqual(state.Visible, want[index]) {
			t.Fatalf("filter %d: got %v want %v", index, state.Visible, want[index])
		}
	}
	if states[2].Message != "显示 0 / 3 条观察" || states[2].NoMatchesHidden {
		t.Fatal("zero-match state not announced")
	}
}
