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
	"unicode/utf8"
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
	uncachedBody, _ := json.Marshal(map[string]any{"text": "订单处于待审核状态时，必须由主管批准后才能继续处理，并记录审批人与时间。", "strict_gate": true, "no_cache": true})
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
	newServerMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/compile/stream", strings.NewReader(string(uncachedBody))).WithContext(context.Background()))
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

// 尾块过短会白烧一次带完整系统提示词的请求：必须并回前一块。
func TestSplitCompileChunksMergesShortTail(t *testing.T) {
	paragraph := strings.Repeat("审", 190) + "。\n"
	text := strings.Repeat(paragraph, 11) + strings.Repeat("尾", 80) + "\n"
	chunks := splitCompileChunks(text, chunkTargetRunes)
	if len(chunks) < 2 {
		t.Fatalf("fixture did not produce multiple chunks: %d", len(chunks))
	}
	if tail := utf8.RuneCountInString(chunks[len(chunks)-1]); tail < chunkMinRunes {
		t.Fatalf("short tail chunk (%d runes) was not merged", tail)
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("tail merge changed source bytes")
	}
}

func TestSplitSectionChunksRespectsBlankLinesAndAutoParallel(t *testing.T) {
	sec1 := "总则：处理订单时每步保留操作日志。\n\n一、资格\n1. 已签收未超过 7 天且未拆封可自助退款。\n" + strings.Repeat("补充条款说明内容用于达到章节分段门槛。\n", 18)
	sec2 := "二、审批与限流\n1. 超过 2000 元禁止线上直接放款；限流每秒 20 次请求。\n" + strings.Repeat("补充执行规则条款说明用于达到章节阈值。\n", 18)
	sec3 := "三、质检边界\n1. 所有写操作保留 180 天快照。\n" + strings.Repeat("职责边界条款说明用于构成完整尾段内容。\n", 14)
	doc := sec1 + "\n" + sec2 + "\n" + sec3
	runes := utf8.RuneCountInString(doc)
	if runes <= autoSectionChunkThreshold || runes > parallelChunkThreshold {
		t.Fatalf("unexpected fixture rune count: %d", runes)
	}
	chunks := splitSectionChunks(doc, autoSectionTargetRunes, autoSectionMinRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected multi-section split, got %d chunk(s)", len(chunks))
	}
	if strings.Join(chunks, "") != doc {
		t.Fatal("section chunking changed source bytes")
	}
	req := enableAutoParallelChunks(CompileReq{Text: doc, Strategy: "auto"})
	if !shouldCompileInChunks(req) {
		t.Fatal("expected auto strategy with multi-section 900+ rune text to enable parallel chunking")
	}
}

func TestSplitSectionChunksSplitsTopLevelHeadingsWithoutBlankLines(t *testing.T) {
	sec1 := "## 变更冻结窗口\n" + strings.Repeat("发布窗口与灰度验证规则说明用于达到章节切分目标长度。\n", 18)
	sec2 := "## 故障分级与响应 SLA\n" + strings.Repeat("核心链路异常必须在五分钟内响应并同步值班群与工单系统。\n", 18) +
		"```sh\n# inside code fence must never split\necho ok\n```\n"
	sec3 := "## 事后复盘要求\n" + strings.Repeat("故障恢复后二十四小时内完成根因分析与改进项跟踪。\n", 12)
	doc := sec1 + sec2 + sec3
	runes := utf8.RuneCountInString(doc)
	if runes <= autoSectionChunkThreshold || runes > parallelChunkThreshold {
		t.Fatalf("unexpected fixture rune count: %d", runes)
	}
	chunks := splitSectionChunks(doc, autoSectionTargetRunes, autoSectionMinRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected heading-based split without blank lines, got %d chunk(s)", len(chunks))
	}
	if strings.Join(chunks, "") != doc {
		t.Fatal("heading section chunking changed source bytes")
	}
	for idx, chunk := range chunks {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Fatalf("chunk %d split a fenced code block: %q", idx, chunk)
		}
	}
	req := enableAutoParallelChunks(CompileReq{Text: doc, Strategy: "auto"})
	if !shouldCompileInChunks(req) {
		t.Fatal("expected auto strategy with Markdown heading sections to enable parallel chunking")
	}
}

func TestEnglishChunkThresholdsAvoidOverSplittingShortEnglishDocs(t *testing.T) {
	shortEN := strings.Repeat("Validate HMAC signatures and reject expired timestamps cleanly.\n\n", 36)
	if r := utf8.RuneCountInString(shortEN); r <= parallelChunkThreshold || r > autoSectionChunkThresholdEN {
		t.Fatalf("unexpected shortEN fixture rune count: %d", r)
	}
	if req := enableAutoParallelChunks(CompileReq{Text: shortEN, Strategy: "auto"}); shouldCompileInChunks(req) {
		t.Fatal("expected ~2300-char pure English doc to stay single-shot rather than over-splitting")
	}

	sec1 := "## Authentication & Tenant Isolation\n" + strings.Repeat("Enforce strict tenant matching and reject malformed payloads immediately.\n", 22)
	sec2 := "## Rate Limiting & Retries\n" + strings.Repeat("Apply token-bucket throttling and bounded exponential backoff for retries.\n", 22)
	longSectionEN := sec1 + "\n" + sec2
	if r := utf8.RuneCountInString(longSectionEN); r <= autoSectionChunkThresholdEN || r > parallelChunkThresholdEN {
		t.Fatalf("unexpected longSectionEN fixture rune count: %d", r)
	}
	if req := enableAutoParallelChunks(CompileReq{Text: longSectionEN, Strategy: "auto"}); !shouldCompileInChunks(req) {
		t.Fatal("expected 2800+ char multi-section English doc to enable parallel chunking")
	}
	if chunks := selectChunksForReq(CompileReq{Text: longSectionEN, Strategy: "auto"}); len(chunks) != 2 {
		t.Fatalf("expected 2 English section chunks, got %d", len(chunks))
	}
}

func TestParallelChunkReportsNonCanceledRootError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 1 && strings.Contains(body.Messages[1].Content, "CHUNK_ONE_SLOW") {
			<-r.Context().Done()
			http.Error(w, "canceled", http.StatusRequestTimeout)
			return
		}
		http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	dialect, err := composeDialect("", nil, "CHUNK_ONE_SLOW CHUNK_TWO_FAIL")
	if err != nil {
		t.Fatal(err)
	}
	prof := Profile{Name: "test", APIKey: "k", BaseURL: srv.URL, Model: "m"}
	_, err = runParallelChunkGeneration(
		context.Background(),
		prof,
		dialect,
		"CHUNK_ONE_SLOW\n\nCHUNK_TWO_FAIL",
		[]string{"CHUNK_ONE_SLOW", "CHUNK_TWO_FAIL"},
		true,
	)
	if err == nil {
		t.Fatal("expected error from failing chunk")
	}
	if !strings.Contains(err.Error(), "第 2 块编译失败") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected chunk 2 root error rather than context canceled, got: %v", err)
	}
}
