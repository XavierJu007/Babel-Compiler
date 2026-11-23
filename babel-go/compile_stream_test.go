package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type streamEvent struct {
	Event string
	Data  map[string]any
}

func parseStreamEvents(t *testing.T, raw string) []streamEvent {
	t.Helper()
	var events []streamEvent
	var current streamEvent
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &current.Data); err != nil {
				t.Fatalf("invalid data frame %q: %v", line, err)
			}
			events = append(events, current)
			current = streamEvent{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// 流式接口按 stage → delta → result 推送；非 stop 的结束原因必须转成 error 事件。
func TestCompileStreamEmitsStagesAndRejectsTruncatedStream(t *testing.T) {
	isolateCompilerState(t)
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: "https://compiler.example/v1", Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}

	finish := atomic.Value{}
	finish.Store("stop")
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); !stream {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"状态=待审\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" ⇒ 主管批准\"},\"finish_reason\":\"" + finish.Load().(string) + "\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer compiler.Close()

	// 把配置里的地址指向测试服务器（compiler 端点必须是完整 URL）。
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"text": "订单处于待审核状态时，必须由主管批准后才能继续处理。", "strict_gate": true})
	req := httptest.NewRequest(http.MethodPost, "/api/compile/stream", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response invalid: status=%d headers=%v", rec.Code, rec.Header())
	}

	events := parseStreamEvents(t, rec.Body.String())
	var stages []string
	sawDelta, sawResult := false, false
	for _, event := range events {
		switch event.Event {
		case "stage":
			stages = append(stages, event.Data["stage"].(string))
		case "delta":
			if event.Data["chunk"] == "状态=待审" || event.Data["chunk"] == " ⇒ 主管批准" {
				sawDelta = true
			}
		case "result":
			sawResult = true
			if event.Data["compiled"] != "状态=待审 ⇒ 主管批准" {
				t.Fatalf("unexpected result payload: %+v", event.Data)
			}
		}
	}
	if !sawDelta || !sawResult {
		t.Fatalf("missing delta/result events: %+v", events)
	}
	if len(stages) < 2 || stages[0] != "generating" || stages[len(stages)-1] != "validating" {
		t.Fatalf("unexpected stage sequence: %v", stages)
	}

	// finish_reason=length 必须按既有防截断规则报错，而不是当成完整候选。
	finish.Store("length")
	rec = httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/compile/stream", strings.NewReader(string(body))))
	events = parseStreamEvents(t, rec.Body.String())
	sawError := false
	for _, event := range events {
		if event.Event == "result" {
			t.Fatal("truncated stream produced a result event")
		}
		if event.Event == "error" && strings.Contains(event.Data["error"].(string), "finish_reason=length") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("truncated stream did not surface an error event: %+v", events)
	}
}

// 长文分块：只在受保护片段之外切分，所有块并行生成后统一做一次全局校验。
func TestParallelChunkCompilePreservesCodeBlocksAndRunsGlobalValidation(t *testing.T) {
	isolateCompilerState(t)

	var genCalls, auditCalls atomic.Int32
	var mu sync.Mutex
	var chunkInputs []string
	var auditOriginal string
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			auditCalls.Add(1)
			var payload struct{ Original, Candidate string }
			_ = json.Unmarshal([]byte(body.Messages[1].Content), &payload)
			mu.Lock()
			auditOriginal = payload.Original
			mu.Unlock()
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		var payload struct{ Original, Candidate string }
		_ = json.Unmarshal([]byte(body.Messages[1].Content), &payload)
		mu.Lock()
		chunkInputs = append(chunkInputs, payload.Original)
		mu.Unlock()
		candidate := "├─关系A⇒结果A\n└─关系B⇒结果B"
		if strings.Contains(payload.Original, "```text") {
			// 分块必须整段保留围栏内容，否则全局字面量校验会拦下候选。
			candidate = "```text\n" + strings.Repeat("keep this code line intact\n", 6) + "```\n├─代码⇒原样保留\n└─其余⇒压缩"
		}
		writeModelContent(w, candidate)
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}

	fence := "```text\n" + strings.Repeat("keep this code line intact\n", 6) + "```\n"
	paragraph := "审批流程要求在提交之前完成全部检查，并在执行之后记录结果，确保每个环节都能追溯到责任人。\n\n"
	source := strings.Repeat(paragraph, 30) + fence + strings.Repeat(paragraph, 10)
	if len([]rune(source)) <= parallelChunkThreshold {
		t.Fatalf("fixture too short: %d runes", len([]rune(source)))
	}

	out, err := doCompileContext(context.Background(), CompileReq{
		Profile:        "compiler",
		Text:           source,
		StrictGate:     true,
		ParallelChunks: true,
	})
	if err != nil {
		t.Fatalf("parallel chunk compile failed: %v", err)
	}
	if !strings.Contains(out.Compiled, "├─") {
		t.Fatalf("chunk candidates were not joined: %q", out.Compiled)
	}
	if genCalls.Load() < 2 {
		t.Fatalf("expected multiple chunk generations, got %d", genCalls.Load())
	}
	if auditCalls.Load() != 1 {
		t.Fatalf("global validation ran %d times; want exactly 1", auditCalls.Load())
	}
	if auditOriginal != source {
		t.Fatalf("validation did not run against the full source (got %d runes, want %d)",
			len([]rune(auditOriginal)), len([]rune(source)))
	}
	mu.Lock()
	defer mu.Unlock()
	for idx, chunk := range chunkInputs {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Fatalf("chunk %d split a fenced block: %q", idx, chunk)
		}
	}
}

// 切分器本身：不切开围栏，且分块数量随长度增长。
func TestSplitCompileChunksKeepsFencesWhole(t *testing.T) {
	fence := "```go\n" + strings.Repeat("var value = compute()\n", 20) + "```\n"
	text := strings.Repeat("普通段落内容，用于撑开分块边界。\n", 80) + fence + strings.Repeat("尾段内容。\n", 40)
	chunks := splitCompileChunks(text, chunkTargetRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Fatal("chunking lost or reordered source bytes")
	}
	for idx, chunk := range chunks {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Fatalf("chunk %d broke a fenced block", idx)
		}
	}
	if small := splitCompileChunks("短文本", chunkTargetRunes); len(small) != 1 {
		t.Fatalf("short text should stay in one chunk, got %d", len(small))
	}
}
