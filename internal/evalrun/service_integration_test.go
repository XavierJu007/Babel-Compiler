package evalrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const serviceCandidate = "certificate expired → service unavailable.\nRestart only after certificate replacement.\n"

// These opt-in tests launch the actual Babel HTTP service. All three upstream
// models are local fixtures; their receipts are transport evidence, not accuracy.
type servicePipelineFixture struct {
	root, endpoint string
	mu             sync.Mutex
	calls          map[string]int
	providerErrors []string
}

func newServicePipelineFixture(t *testing.T) *servicePipelineFixture {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("BABEL_TEST_BINARY"))
	if binary == "" {
		t.Skip("set BABEL_TEST_BINARY to a built Babel binary for HTTP integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &servicePipelineFixture{root: t.TempDir(), calls: make(map[string]int)}
	provider := httptest.NewServer(http.HandlerFunc(fixture.serveModel))
	t.Cleanup(provider.Close)
	profiles := Record{}
	for _, name := range []string{"compiler", "reader", "checker"} {
		model := name + "-fixture"
		profiles[name] = Record{"base_url": provider.URL, "api_key": "mock-" + model, "model": model}
	}
	config, err := json.Marshal(Record{"active_profile": "compiler", "profiles": profiles})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "babel.config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, binary, "-serve", "127.0.0.1:0")
	command.Dir = fixture.root
	for _, setting := range os.Environ() {
		key, _, _ := strings.Cut(setting, "=")
		if key != "HOME" && key != "USERPROFILE" && !strings.HasPrefix(key, "BABEL_") {
			command.Env = append(command.Env, setting)
		}
	}
	command.Env = append(command.Env, "HOME="+fixture.root, "USERPROFILE="+fixture.root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		t.Fatalf("start Babel: %v", err)
	}

	started := make(chan string, 1)
	readDone, waitDone := make(chan struct{}), make(chan struct{})
	var stateMu sync.Mutex
	var logs strings.Builder
	var waitErr error
	address := regexp.MustCompile(`http://127\.0\.0\.1:[1-9][0-9]*`)
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			stateMu.Lock()
			if logs.Len() < 32768 {
				logs.WriteString(line + "\n")
			}
			stateMu.Unlock()
			if endpoint := address.FindString(line); endpoint != "" {
				select {
				case started <- endpoint:
				default:
				}
			}
		}
	}()
	go func() {
		err := command.Wait()
		stateMu.Lock()
		waitErr = err
		stateMu.Unlock()
		close(waitDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			t.Error("Babel process did not stop after cancellation")
		}
		_ = stdout.Close()
		select {
		case <-readDone:
		case <-time.After(time.Second):
			t.Error("Babel log reader did not stop")
		}
	})
	select {
	case fixture.endpoint = <-started:
		return fixture
	case <-waitDone:
		stateMu.Lock()
		defer stateMu.Unlock()
		t.Fatalf("Babel exited before starting its HTTP listener: %v\n%s", waitErr, logs.String())
	case <-time.After(10 * time.Second):
		stateMu.Lock()
		defer stateMu.Unlock()
		t.Fatalf("Babel did not report its listening address:\n%s", logs.String())
	}
	return nil
}

func (fixture *servicePipelineFixture) serveModel(w http.ResponseWriter, r *http.Request) {
	reject := func(reason string) {
		fixture.mu.Lock()
		fixture.providerErrors = append(fixture.providerErrors, reason)
		fixture.mu.Unlock()
		http.Error(w, "fixture rejected the request", http.StatusInternalServerError)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxResponseBytes))
	if err != nil {
		reject(err.Error())
		return
	}
	body, err := DecodeRecord(raw)
	if err != nil {
		reject(err.Error())
		return
	}
	model := textValue(body["model"])
	fixture.mu.Lock()
	fixture.calls[model]++
	fixture.mu.Unlock()
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer mock-"+model {
		reject("unexpected method or fixture authorization")
		return
	}
	messages := array(body["messages"])
	if len(messages) != 2 {
		reject("expected a fresh two-message model request")
		return
	}
	input := textValue(object(messages[1])["content"])
	answer := serviceCandidate
	switch model {
	case "compiler-fixture":
		if !strings.Contains(input, "certificate expired") {
			reject("compiler did not receive the selected source")
			return
		}
	case "reader-fixture":
		payload, err := DecodeRecord([]byte(input))
		if err != nil || len(payload) != 1 || payload["candidate"] != serviceCandidate {
			reject("reader received source/context or a changed candidate")
			return
		}
		reading := Record{"schema_version": 1, "entities": []string{"certificate", "service"},
			"relations": []Record{
				{"subject": "certificate", "relation": "expired", "object": "", "conditions": []string{},
					"scope": "cause of service unavailability", "modality": "fact", "negated": false,
					"quantities": []any{}, "evidence": "certificate expired → service unavailable."},
				{"subject": "", "relation": "Restart", "object": "", "conditions": []string{"only after certificate replacement"},
					"scope": "", "modality": "obligation", "negated": false,
					"quantities": []any{}, "evidence": "Restart only after certificate replacement."},
			}, "ambiguities": []string{}}
		encoded, _ := json.Marshal(reading)
		answer = string(encoded)
	case "checker-fixture":
		payload, err := DecodeRecord([]byte(input))
		if err != nil || len(payload) != 2 || object(payload["reading"]) == nil || !strings.Contains(textValue(payload["original"]), "certificate expired") {
			reject("checker did not receive only the source and independent reading")
			return
		}
		answer = `{"pass":true,"missing":[]}`
	default:
		reject("unexpected configured fixture model: " + model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Record{"choices": []Record{{"finish_reason": "stop", "message": Record{"content": answer}}}})
}

func (fixture *servicePipelineFixture) runPipeline(t *testing.T, budget int) (string, []Record) {
	t.Helper()
	output := filepath.Join(fixture.root, "run.jsonl")
	args := []string{"--live", "pipeline", "--endpoint", fixture.endpoint, "--profile", "compiler", "--validation-profile", "checker",
		"--validation-mode", "blind", "--reader-profile", "reader", "--token-encoding", "o200k_base",
		"--case-id", "en-cause-and-recovery", "--limit", "1", "--max-requests", strconv.Itoa(budget), "--timeout", "5", "--output", output}
	var stdout, stderr bytes.Buffer
	if code := RunCLI(args, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline exit %d: %s\n%s", code, stderr.String(), stdout.String())
	}
	fixture.mu.Lock()
	errors := append([]string{}, fixture.providerErrors...)
	fixture.mu.Unlock()
	if len(errors) != 0 {
		t.Fatalf("provider fixture rejected requests: %v", errors)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var records []Record
	for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		record, err := DecodeRecord(line)
		if err != nil {
			t.Fatalf("output line %d: %v", i+1, err)
		}
		records = append(records, record)
	}
	if len(records) != 3 {
		t.Fatalf("expected manifest, one observation and summary; got %d", len(records))
	}
	return output, records
}

func (fixture *servicePipelineFixture) assertCalls(t *testing.T, want map[string]int) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !reflect.DeepEqual(fixture.calls, want) {
		t.Fatalf("upstream calls=%v; want %v", fixture.calls, want)
	}
}

func TestRealServicePipelineToHTMLReport(t *testing.T) {
	fixture := newServicePipelineFixture(t)
	output, records := fixture.runPipeline(t, 2)
	manifest, record, summary := records[0], records[1], records[2]
	if record["candidate"] != serviceCandidate || record["result"] != "passed" || object(record["validation"])["method"] != "blind_read" {
		t.Fatalf("unexpected validation observation: %+v", record)
	}
	if object(record["stats"])["measurement_encoding"] != "o200k_base" || object(record["measurement"])["status"] != "included" || object(record["selection"])["output"] != "candidate" {
		t.Fatalf("missing exact measurement or candidate selection: %+v", record)
	}
	requests, valid := integer(summary["requests"])
	if manifest["selection_scope"] != "evaluation_rule_only" || !valid || requests != 2 || summary["semantic_accuracy"] != "not_established" {
		t.Fatalf("misleading or incorrect pipeline summary: %+v", summary)
	}
	fixture.assertCalls(t, map[string]int{"compiler-fixture": 1, "reader-fixture": 1, "checker-fixture": 1})
	report := filepath.Join(fixture.root, "report.html")
	if err := WriteReport(output, report); err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"en-cause-and-recovery", "o200k_base", "certificate expired"} {
		if !bytes.Contains(rendered, []byte(expected)) {
			t.Errorf("HTML report omitted %q", expected)
		}
	}
}

func TestRealServiceBudgetKeepsCandidateUnvalidated(t *testing.T) {
	fixture := newServicePipelineFixture(t)
	_, records := fixture.runPipeline(t, 1)
	record, summary := records[1], records[2]
	if record["candidate"] != serviceCandidate || record["result"] != "not_validated" || object(record["selection"])["output"] != "source" {
		t.Fatalf("budget exhaustion discarded or trusted an unvalidated candidate: %+v", record)
	}
	requests, requestsOK := integer(summary["requests"])
	validated, validatedOK := integer(summary["validate_requests"])
	if !requestsOK || requests != 1 || !validatedOK || validated != 0 || summary["validation_pass_rate"] != nil {
		t.Fatalf("budget exhaustion produced a validation receipt: %+v", summary)
	}
	fixture.assertCalls(t, map[string]int{"compiler-fixture": 1})
}
