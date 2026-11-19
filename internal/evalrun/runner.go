package evalrun

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 2 * 1024 * 1024

type options struct {
	cases, live, endpoint, profile, validationProfile, validationMode string
	mode, tokenEncoding, output                                       string
	readerProfile                                                     *string
	caseIDs                                                           []string
	limit, maxRequests                                                int
	timeout                                                           time.Duration
}

type repeatedIDs []string

func (values *repeatedIDs) String() string { return strings.Join(*values, ",") }
func (values *repeatedIDs) Set(value string) error {
	*values = append(*values, value)
	return nil
}

// RunCLI never loads application credentials. Network requests require explicit
// live mode, endpoint, named profile, request budget and a fresh output file.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "report" {
		return reportCLI(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("babel-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o options
	var reader string
	var ids repeatedIDs
	flags.StringVar(&o.cases, "cases", "", "optional JSONL case file; default uses the bundled examples")
	flags.StringVar(&o.live, "live", "", "explicit network operation: compile, validate or pipeline; default offline")
	flags.StringVar(&o.endpoint, "endpoint", "", "Babel base URL, required for live requests")
	flags.StringVar(&o.profile, "profile", "", "explicit saved Babel profile")
	flags.StringVar(&o.validationProfile, "validation-profile", "", "pipeline comparator profile; defaults to profile")
	flags.StringVar(&reader, "reader-profile", "", "blind reader override; an explicit empty value clears the inherited reader")
	flags.StringVar(&o.validationMode, "validation-mode", "compare", "compare or blind")
	flags.StringVar(&o.mode, "mode", "mixed", "deprecated builtin alias; product always uses mixed")
	flags.StringVar(&o.tokenEncoding, "token-encoding", "", "estimate, cl100k_base or o200k_base")
	flags.Var(&ids, "case-id", "include this case ID; repeatable")
	flags.IntVar(&o.limit, "limit", 2, "maximum cases for live mode, in file order")
	flags.IntVar(&o.maxRequests, "max-requests", 0, "explicit hard HTTP request budget; no retries")
	seconds := flags.Float64("timeout", 60, "timeout for each HTTP request, in seconds")
	flags.StringVar(&o.output, "output", "", "new JSONL output file; required for live mode")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "evaluation error:", err)
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments"))
	}
	provided := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["reader-profile"] {
		o.readerProfile = &reader
	}
	o.caseIDs = ids
	if o.limit <= 0 || *seconds <= 0 || math.IsNaN(*seconds) || math.IsInf(*seconds, 0) ||
		*seconds >= float64(int64(^uint64(0)>>1))/float64(time.Second) {
		return fail(fmt.Errorf("--limit and --timeout must be positive and finite"))
	}
	o.timeout = time.Duration(*seconds * float64(time.Second))
	if o.timeout <= 0 {
		return fail(fmt.Errorf("--timeout must be at least one nanosecond"))
	}
	if o.validationMode != "compare" && o.validationMode != "blind" {
		return fail(fmt.Errorf("--validation-mode must be compare or blind"))
	}
	if o.tokenEncoding != "" && o.tokenEncoding != "estimate" && o.tokenEncoding != "cl100k_base" && o.tokenEncoding != "o200k_base" {
		return fail(fmt.Errorf("unsupported --token-encoding"))
	}
	o.mode = strings.TrimSuffix(strings.TrimSpace(o.mode), ".md")
	if o.mode == "" {
		o.mode = "mixed"
	}
	switch o.mode {
	case "mixed", "extreme", "token", "standard", "code-audit", "en-terse":
	default:
		return fail(fmt.Errorf("only legacy builtin --mode aliases are supported; the policy is fixed mixed"))
	}
	cases, datasetSHA, err := LoadCases(o.cases)
	if err != nil {
		return fail(err)
	}
	known := map[string]bool{}
	for _, c := range cases {
		known[textValue(c["id"])] = true
	}
	selectedIDs := map[string]bool{}
	for _, id := range o.caseIDs {
		if !known[id] {
			return fail(fmt.Errorf("--case-id contains an unknown ID: %s", id))
		}
		selectedIDs[id] = true
	}
	if o.live == "" {
		for _, key := range []string{"endpoint", "profile", "reader-profile", "max-requests", "token-encoding", "validation-profile"} {
			if provided[key] {
				return fail(fmt.Errorf("network/model options require explicit --live"))
			}
		}
		mutants := 0
		for _, c := range cases {
			mutants += len(array(c["invalid_candidates"]))
		}
		summary := Record{"operation": "offline_structure_check", "structure_checks": "passed",
			"cases": len(cases), "invalid_candidates": mutants, "dataset_sha256": datasetSHA,
			"annotation_status": AnnotationStatus, "network_requests": 0, "semantic_quality": "not_evaluated"}
		if o.output != "" {
			file, err := createOutput(o.output)
			if err != nil {
				return fail(err)
			}
			writeErr := writeRecord(file, summary)
			closeErr := file.Close()
			if writeErr != nil {
				return fail(writeErr)
			}
			if closeErr != nil {
				return fail(closeErr)
			}
		}
		if err := writeRecord(stdout, summary); err != nil {
			return fail(err)
		}
		return 0
	}
	if o.live != "compile" && o.live != "validate" && o.live != "pipeline" {
		return fail(fmt.Errorf("--live must be compile, validate or pipeline"))
	}
	if strings.TrimSpace(o.profile) == "" || strings.TrimSpace(o.endpoint) == "" {
		return fail(fmt.Errorf("--live requires explicit --endpoint and --profile"))
	}
	if !provided["max-requests"] || o.maxRequests <= 0 {
		return fail(fmt.Errorf("--live requires a positive explicit --max-requests"))
	}
	if o.output == "" {
		return fail(fmt.Errorf("--live requires --output"))
	}
	if o.readerProfile != nil && (o.live == "compile" || o.validationMode != "blind") {
		return fail(fmt.Errorf("--reader-profile requires --live validate/pipeline --validation-mode blind"))
	}
	if provided["validation-profile"] && (o.live != "pipeline" || strings.TrimSpace(o.validationProfile) == "") {
		return fail(fmt.Errorf("--validation-profile requires --live pipeline and a nonempty profile name"))
	}
	if provided["token-encoding"] && o.live == "validate" {
		return fail(fmt.Errorf("--token-encoding is only used with --live compile/pipeline"))
	}
	o.endpoint, err = validateEndpoint(o.endpoint)
	if err != nil {
		return fail(err)
	}
	selected := []Record{}
	for _, c := range cases {
		if len(selectedIDs) == 0 || selectedIDs[textValue(c["id"])] {
			selected = append(selected, c)
			if len(selected) == o.limit {
				break
			}
		}
	}
	file, err := createOutput(o.output)
	if err != nil {
		return fail(err)
	}
	summary, runErr := runLive(o, selected, datasetSHA, file)
	closeErr := file.Close()
	if runErr != nil {
		return fail(runErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if err := writeRecord(stdout, summary); err != nil {
		return fail(err)
	}
	errors, _ := integer(summary["error"])
	unknown, _ := integer(summary["unknown"])
	if errors != 0 || unknown != 0 {
		return 2
	}
	return 0
}

func reportCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, "usage: babel-eval report <run.jsonl> -output <new-report.html>")
		if len(args) == 0 {
			return 1
		}
		return 0
	}
	flags := flag.NewFlagSet("babel-eval report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "new self-contained HTML report")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	if *output == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "report requires one input file and -output")
		return 1
	}
	if err := WriteReport(args[0], *output); err != nil {
		fmt.Fprintln(stderr, "report error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Wrote", *output)
	return 0
}

func validateEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("--endpoint must be an explicit http(s) Babel URL without credentials, query or fragment")
	}
	return strings.TrimRight(endpoint, "/"), nil
}

func requestJSON(endpoint string, payload Record, timeout time.Duration) (int, Record) {
	var data bytes.Buffer
	if err := writeRecord(&data, payload); err != nil {
		return 0, Record{"response_error": "invalid_request"}
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, &data)
	if err != nil {
		return 0, Record{"response_error": "transport_error"}
	}
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, Record{"response_error": "transport_error"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, Record{"response_error": "transport_error"}
	}
	if len(raw) > maxResponseBytes {
		return response.StatusCode, Record{"response_error": "response_too_large"}
	}
	body, err := DecodeRecord(raw)
	if err != nil {
		return response.StatusCode, Record{"response_error": "invalid_json"}
	}
	return response.StatusCode, body
}

func compilePayload(o options, c Record) Record {
	payload := Record{"text": c["source"], "profile": o.profile, "mode": o.mode, "domains": c["domains"],
		"verify": false, "strict_gate": false, "no_cache": true}
	if o.tokenEncoding != "" {
		payload["token_encoding"] = o.tokenEncoding
	}
	return payload
}

func validatePayload(o options, source, candidate any, profile string) Record {
	payload := Record{"text": source, "candidate": candidate, "profile": profile, "validation_mode": o.validationMode}
	if o.readerProfile != nil {
		payload["reader_profile"] = *o.readerProfile
	}
	return payload
}

func newManifest(o options, cases []Record, datasetSHA string, planned, format int) Record {
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, textValue(c["id"]))
	}
	var expectedPolicy any
	if o.live != "compile" {
		expectedPolicy = ComparePolicy
		if o.validationMode == "blind" {
			expectedPolicy = BlindPolicy
		}
	}
	var reader any
	if o.readerProfile != nil {
		reader = *o.readerProfile
	}
	var encoding any
	if o.tokenEncoding != "" {
		encoding = o.tokenEncoding
	}
	return Record{"record_type": "manifest", "format_version": format, "started_at_utc": time.Now().UTC().Format(time.RFC3339Nano),
		"annotation_status": AnnotationStatus, "dataset_sha256": datasetSHA, "operation": o.live,
		"endpoint": o.endpoint, "profile": o.profile, "reader_profile": reader, "validation_mode": o.validationMode,
		"expected_validation_policy": expectedPolicy, "output_preset": o.mode, "token_encoding_requested": encoding,
		"planned_requests": planned, "max_requests": o.maxRequests, "selected_case_ids": ids,
		"upstream_model_identity": "not_independently_verified",
		"evidence_scope":          "Babel API observations; HTTP budget is not an upstream-model-call budget"}
}

func elapsed(start time.Time) float64 { return math.Round(time.Since(start).Seconds()*1e6) / 1e6 }

func missingLiterals(c Record, candidate string) Record {
	missing := []string{}
	for _, raw := range array(c["protected_literals"]) {
		literal := textValue(raw)
		if !strings.Contains(candidate, literal) {
			missing = append(missing, literal)
		}
	}
	return Record{"missing": missing, "semantic_equivalence": "not_evaluated"}
}

func httpStatus(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

func runLive(o options, cases []Record, datasetSHA string, writer io.Writer) (Record, error) {
	if o.live == "pipeline" {
		return runPipeline(o, cases, datasetSHA, writer)
	}
	type item struct {
		c                       Record
		id, candidate, expected string
	}
	plan := []item{}
	for _, c := range cases {
		if o.live == "compile" {
			plan = append(plan, item{c: c, id: "generated"})
		} else {
			plan = append(plan, item{c, "valid", textValue(c["valid_candidate"]), "passed"})
			for _, raw := range array(c["invalid_candidates"]) {
				m := object(raw)
				plan = append(plan, item{c, textValue(m["id"]), textValue(m["text"]), "failed"})
			}
		}
	}
	if err := writeRecord(writer, newManifest(o, cases, datasetSHA, len(plan), 1)); err != nil {
		return nil, err
	}
	counts := Record{"requests": int64(0), "api_success": int64(0), "error": int64(0), "unknown": int64(0)}
	matrix := Record{"draft_valid": Record{"passed": int64(0), "failed": int64(0)}, "draft_invalid": Record{"passed": int64(0), "failed": int64(0)}}
	for i, entry := range plan {
		if i >= o.maxRequests {
			break
		}
		payload, route := compilePayload(o, entry.c), "/api/compile"
		if o.live == "validate" {
			payload, route = validatePayload(o, entry.c["source"], entry.candidate, o.profile), "/api/validate"
		}
		started := time.Now()
		status, body := requestJSON(o.endpoint+route, payload, o.timeout)
		increment(counts, "requests", 1)
		record := Record{"record_type": "observation", "case_id": entry.c["id"], "candidate_id": entry.id,
			"http_status": httpStatus(status), "elapsed_seconds": elapsed(started), "source": entry.c["source"],
			"expected_relations": entry.c["expected_relations"], "protected_literals": entry.c["protected_literals"]}
		var category string
		if o.live == "compile" {
			category = "unknown"
			if status != http.StatusOK {
				category = "error"
			} else if nonempty(body["compiled"]) {
				category = "api_success"
				record["candidate"] = body["compiled"]
				record["literal_presence_check"] = missingLiterals(entry.c, textValue(body["compiled"]))
				for _, field := range []string{"stats", "usage"} {
					if object(body[field]) != nil {
						record[field] = body[field]
					}
				}
				_, record["validation"] = ValidationObservation(status, body, "compare")
			}
			record["semantic_quality"] = "not_evaluated"
		} else {
			category, record["validation"] = ValidationObservation(status, body, o.validationMode)
			record["candidate"], record["draft_expected_validation"] = entry.candidate, entry.expected
			if category == "passed" || category == "failed" {
				row := "draft_invalid"
				if entry.expected == "passed" {
					row = "draft_valid"
				}
				increment(object(matrix[row]), category, 1)
				increment(counts, "api_success", 1)
			}
		}
		record["result"] = category
		if _, ok := counts[category]; ok {
			increment(counts, category, 1)
		}
		if err := writeRecord(writer, record); err != nil {
			return nil, err
		}
	}
	summary := Record{"record_type": "summary", "operation": o.live, "planned_requests": len(plan),
		"annotation_status": AnnotationStatus, "semantic_accuracy": "not_established", "upstream_model_identity": "not_independently_verified"}
	for field, value := range counts {
		summary[field] = value
	}
	requests, _ := integer(counts["requests"])
	summary["skipped_by_budget"] = int64(len(plan)) - requests
	if o.live == "validate" {
		var decided, agreement int64
		for row, raw := range matrix {
			for result, value := range object(raw) {
				n, _ := integer(value)
				decided += n
				if (row == "draft_valid" && result == "passed") || (row == "draft_invalid" && result == "failed") {
					agreement += n
				}
			}
		}
		summary["draft_label_confusion_matrix"], summary["draft_label_agreement_denominator"] = matrix, decided
		summary["draft_label_agreement"] = nil
		if decided != 0 {
			summary["draft_label_agreement"] = float64(agreement) / float64(decided)
		}
		summary["excluded_from_agreement"] = Record{"error": counts["error"], "unknown": counts["unknown"]}
	}
	return summary, writeRecord(writer, summary)
}

func runPipeline(o options, cases []Record, datasetSHA string, writer io.Writer) (Record, error) {
	validationProfile := o.validationProfile
	if validationProfile == "" {
		validationProfile = o.profile
	}
	manifest := newManifest(o, cases, datasetSHA, 2*len(cases), 2)
	manifest["validation_profile"], manifest["planned_requests_kind"], manifest["selection_scope"] = validationProfile, "upper_bound", "evaluation_rule_only"
	if err := writeRecord(writer, manifest); err != nil {
		return nil, err
	}
	counts := Record{}
	for _, key := range []string{"requests", "compile_requests", "validate_requests", "compile_successes", "api_success",
		"passed", "failed", "error", "unknown", "not_validated", "skipped_by_budget", "skipped_after_compile_failure"} {
		counts[key] = int64(0)
	}
	requestStage := func(name string, payload Record) (Record, Record) {
		started := time.Now()
		status, body := requestJSON(o.endpoint+"/api/"+name, payload, o.timeout)
		increment(counts, "requests", 1)
		increment(counts, name+"_requests", 1)
		return body, Record{"http_status": httpStatus(status), "elapsed_seconds": elapsed(started)}
	}
	skipped := func(reason string) Record {
		return Record{"result": "not_run", "http_status": nil, "elapsed_seconds": nil, "skip_reason": reason}
	}
	budgetEmpty := func() bool { n, _ := integer(counts["requests"]); return n >= int64(o.maxRequests) }
	groups, excluded := map[string]Record{}, map[string]int{}
	selectionCounts := Record{"candidate": int64(0), "source": int64(0)}
	for _, c := range cases {
		started := time.Now()
		stages := Record{}
		validation := Record{"requested_mode": o.validationMode, "status": "not_run"}
		record := Record{"record_type": "observation", "operation": "pipeline", "case_id": c["id"], "candidate_id": "generated",
			"source": c["source"], "candidate": nil, "stats": nil, "expected_relations": c["expected_relations"],
			"protected_literals": c["protected_literals"], "stages": stages}
		category := "not_validated"
		if budgetEmpty() {
			stages["compile"], stages["validate"] = skipped("budget_exhausted"), skipped("budget_exhausted")
			validation["receipt_check"] = "budget_exhausted"
			increment(counts, "skipped_by_budget", 2)
		} else {
			body, stage := requestStage("compile", compilePayload(o, c))
			status, _ := integer(stage["http_status"])
			success := status == http.StatusOK && nonempty(body["compiled"])
			stage["result"] = "unknown"
			if status != http.StatusOK {
				stage["result"] = "error"
			} else if success {
				stage["result"] = "api_success"
			}
			stages["compile"] = stage
			if !success {
				category = textValue(stage["result"])
				stages["validate"], validation["receipt_check"] = skipped("compile_unavailable"), "compile_unavailable"
				increment(counts, "skipped_after_compile_failure", 1)
			} else {
				increment(counts, "compile_successes", 1)
				increment(counts, "api_success", 1)
				record["candidate"] = body["compiled"]
				for _, field := range []string{"stats", "usage"} {
					if object(body[field]) != nil {
						record[field] = body[field]
					}
				}
				record["literal_presence_check"] = missingLiterals(c, textValue(body["compiled"]))
				if budgetEmpty() {
					stages["validate"], validation["receipt_check"] = skipped("budget_exhausted"), "budget_exhausted"
					increment(counts, "skipped_by_budget", 1)
				} else {
					body, stage = requestStage("validate", validatePayload(o, c["source"], record["candidate"], validationProfile))
					status, _ = integer(stage["http_status"])
					category, validation = ValidationObservation(int(status), body, o.validationMode)
					stage["result"], stages["validate"] = category, stage
					if category == "passed" || category == "failed" {
						increment(counts, "api_success", 1)
					}
				}
			}
		}
		increment(counts, category, 1)
		record["validation"], record["result"], record["elapsed_seconds"] = validation, category, elapsed(started)
		measurement, selection := PipelineMeasurement(record["stats"], category, record["candidate"] != nil)
		record["measurement"], record["selection"], record["semantic_accuracy"] = measurement, selection, "not_established"
		addPipelineMeasurement(groups, excluded, measurement, selection)
		increment(selectionCounts, textValue(selection["output"]), 1)
		if err := writeRecord(writer, record); err != nil {
			return nil, err
		}
	}
	passed, _ := integer(counts["passed"])
	failed, _ := integer(counts["failed"])
	decided := passed + failed
	var rate any
	if decided > 0 {
		rate = float64(passed) / float64(decided)
	}
	summary := Record{"record_type": "summary", "operation": "pipeline", "cases": len(cases),
		"planned_requests": 2 * len(cases), "planned_requests_kind": "upper_bound",
		"validation_decided": decided, "validation_pass_rate": rate,
		"excluded_from_validation": Record{"error": counts["error"], "unknown": counts["unknown"], "not_validated": counts["not_validated"]},
		"selection_counts":         selectionCounts, "selection_scope": "evaluation_rule_only",
		"measurement_groups": sortedMeasurementGroups(groups), "measurement_excluded": excluded,
		"measurement_evidence": "service_reported_text_counts", "annotation_status": AnnotationStatus,
		"semantic_accuracy": "not_established", "billing_cost": "not_measured", "upstream_model_identity": "not_independently_verified"}
	for field, value := range counts {
		summary[field] = value
	}
	return summary, writeRecord(writer, summary)
}
