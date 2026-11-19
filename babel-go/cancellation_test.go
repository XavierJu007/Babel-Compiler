package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitCancellationSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestCanceledValidationAPIStopsCurrentModelAndSkipsLaterStages(t *testing.T) {
	for _, mode := range []string{"compare", "blind"} {
		t.Run(mode, func(t *testing.T) {
			isolateDomainIntegration(t)
			started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				if calls.Add(1) != 1 {
					t.Error("canceled validation started another model request")
					return
				}
				close(started)
				select {
				case <-r.Context().Done():
					close(stopped)
				case <-release:
				}
			}))
			defer provider.Close()
			defer close(release)
			body, _ := json.Marshal(map[string]any{
				"text": "系统允许删除日志。", "candidate": "系统允许删除日志。",
				"validation_mode": mode, "base_url": provider.URL, "api_key": "test", "model": "test",
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(string(body))).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { newServerMux().ServeHTTP(response, request); close(done) }()
			waitCancellationSignal(t, started, "validation model")
			cancel()
			waitCancellationSignal(t, done, "canceled validation handler")
			waitCancellationSignal(t, stopped, "provider cancellation")
			var result struct {
				Validation ValidationResult `json:"validation"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Validation.Status != "error" || calls.Load() != 1 {
				t.Fatalf("cancellation became a pass/rejection: %s %v", response.Body.String(), err)
			}
			if mode == "blind" && (result.Validation.Stages[1].Status != "error" || result.Validation.Stages[2].Status != "not_run") {
				t.Fatalf("later stage ran after cancellation: %+v", result.Validation.Stages)
			}
		})
	}
}

func TestCanceledNoCacheCompileAPIStopsGenerationWithoutAudit(t *testing.T) {
	isolateDomainIntegration(t)
	started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		if calls.Add(1) != 1 {
			t.Error("audit started after canceled generation")
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)
	body, _ := json.Marshal(CompileReq{Text: "系统允许删除日志。", NoCache: true, StrictGate: true,
		ValidationMode: "blind", BaseURL: provider.URL, APIKey: "test", Model: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/compile", strings.NewReader(string(body))).WithContext(ctx)
	response, done := httptest.NewRecorder(), make(chan struct{})
	go func() { newServerMux().ServeHTTP(response, request); close(done) }()
	waitCancellationSignal(t, started, "generation")
	cancel()
	waitCancellationSignal(t, done, "canceled compile handler")
	waitCancellationSignal(t, stopped, "canceled generator")
	if calls.Load() != 1 {
		t.Fatal("canceled no-cache compilation called a later stage")
	}
	cacheMutex.RLock()
	entries := len(diskCache)
	cacheMutex.RUnlock()
	if entries != 0 {
		t.Fatal("canceled no-cache generation wrote a candidate")
	}
}

func TestCanceledDefaultCacheCallersStopGenerationBeforeOutlineRepair(t *testing.T) {
	for _, callers := range []int{1, 2} {
		name := "sole caller"
		if callers == 2 {
			name = "all shared callers"
		}
		t.Run(name, func(t *testing.T) {
			isolateDomainIntegration(t)
			const source = "一、资格判定\n检查订单状态并记录判定依据。\n二、审批链\n检查退款金额并记录审批结果。"
			started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-r.Context().Done():
						close(stopped)
					case <-release:
					}
				}
				// If abandoned work kept running, this retained outline would
				// otherwise cause a second model request for restructuring.
				writeModelContent(w, source)
			}))
			defer provider.Close()
			defer close(release)
			req := CompileReq{Text: source, StrictGate: true, BaseURL: provider.URL, APIKey: "test", Model: "test"}
			var cancels []context.CancelFunc
			var results []chan error
			defer func() {
				for _, cancel := range cancels {
					cancel()
				}
			}()
			for i := 0; i < callers; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)
				result := make(chan error, 1)
				results = append(results, result)
				go func() {
					_, err := doCompileContext(ctx, req)
					result <- err
				}()
				if i == 0 {
					waitCancellationSignal(t, started, "default-cache generation")
				}
			}
			var call *flightCall
			deadline := time.Now().Add(2 * time.Second)
			for call == nil && time.Now().Before(deadline) {
				compileFlight.mu.Lock()
				for _, current := range compileFlight.calls {
					if current.active == callers {
						call = current
						break
					}
				}
				compileFlight.mu.Unlock()
				runtime.Gosched()
			}
			if call == nil {
				t.Fatal("callers did not join the shared generation")
			}
			for _, cancel := range cancels {
				cancel()
			}
			for _, result := range results {
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled caller returned %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("canceled caller kept waiting for generation")
				}
			}
			waitCancellationSignal(t, stopped, "canceled shared upstream request")
			waitCancellationSignal(t, call.done, "abandoned generation completion")
			if calls.Load() != 1 {
				t.Fatalf("abandoned generation started repair or validation: calls=%d", calls.Load())
			}
			cacheMutex.RLock()
			entries := len(diskCache)
			cacheMutex.RUnlock()
			if entries != 0 {
				t.Fatal("abandoned generation populated the candidate cache")
			}
			compileFlight.mu.Lock()
			inflight := len(compileFlight.calls)
			compileFlight.mu.Unlock()
			if inflight != 0 {
				t.Fatal("abandoned generation retained its flight entry")
			}
		})
	}
}

func TestCanceledFlightCannotDeleteReplacementForSameKey(t *testing.T) {
	var group flightGroup
	oldStarted, oldStopped, oldRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	newStarted, newRelease := make(chan struct{}), make(chan struct{})
	var oldOnce, newOnce sync.Once
	unblockOld := func() { oldOnce.Do(func() { close(oldRelease) }) }
	unblockNew := func() { newOnce.Do(func() { close(newRelease) }) }
	defer unblockOld()
	defer unblockNew()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldResult := make(chan error, 1)
	go func() {
		_, _, err := group.Do(ctx, "same-key", func(work context.Context) (CompileResp, error) {
			close(oldStarted)
			<-work.Done()
			close(oldStopped)
			<-oldRelease // Simulate late teardown after cancellation.
			return CompileResp{}, work.Err()
		})
		oldResult <- err
	}()
	waitCancellationSignal(t, oldStarted, "first generation")
	group.mu.Lock()
	oldCall := group.calls["same-key"]
	group.mu.Unlock()
	cancel()
	select {
	case err := <-oldResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old caller did not cancel")
	}
	waitCancellationSignal(t, oldStopped, "shared context cancellation")
	type result struct {
		out    CompileResp
		shared bool
		err    error
	}
	newResult := make(chan result, 1)
	go func() {
		out, shared, err := group.Do(context.Background(), "same-key", func(work context.Context) (CompileResp, error) {
			close(newStarted)
			<-newRelease
			return CompileResp{Compiled: "replacement"}, work.Err()
		})
		newResult <- result{out, shared, err}
	}()
	waitCancellationSignal(t, newStarted, "replacement generation")
	group.mu.Lock()
	newCall := group.calls["same-key"]
	group.mu.Unlock()
	unblockOld()
	waitCancellationSignal(t, oldCall.done, "late old generation completion")
	group.mu.Lock()
	stillCurrent := group.calls["same-key"] == newCall
	group.mu.Unlock()
	if !stillCurrent {
		t.Fatal("late completion deleted another generation's entry")
	}
	unblockNew()
	select {
	case got := <-newResult:
		if got.err != nil || got.shared || got.out.Compiled != "replacement" {
			t.Fatalf("replacement damaged by old flight: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement generation did not finish")
	}
}

func TestCanceledSharedCallerLeavesOtherRequestsAndValidationIndependent(t *testing.T) {
	for _, canceledCaller := range []string{"leader", "follower"} {
		t.Run(canceledCaller, func(t *testing.T) {
			isolateDomainIntegration(t)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var generations, audits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				if body.Messages[0].Content == verifyGatePrompt {
					audits.Add(1)
					writeModelContent(w, `{"pass":true,"missing":[]}`)
					return
				}
				if generations.Add(1) == 1 {
					close(started)
				}
				<-release
				writeModelContent(w, "系统允许删除日志。")
			}))
			defer provider.Close()
			defer unblock()
			req := CompileReq{Text: "这里的规则是：系统允许删除日志。", StrictGate: true, BaseURL: provider.URL, APIKey: "test", Model: "test"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				out CompileResp
				err error
			}
			leader, follower := make(chan result, 1), make(chan result, 1)
			leaderCtx, followerCtx := context.Background(), context.Background()
			if canceledCaller == "leader" {
				leaderCtx = ctx
			} else {
				followerCtx = ctx
			}
			go func() { out, err := doCompileContext(leaderCtx, req); leader <- result{out, err} }()
			waitCancellationSignal(t, started, "shared generator")
			go func() { out, err := doCompileContext(followerCtx, req); follower <- result{out, err} }()
			joined := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				compileFlight.mu.Lock()
				for _, call := range compileFlight.calls {
					joined = joined || call.waiters > 0
				}
				compileFlight.mu.Unlock()
				if joined {
					break
				}
				runtime.Gosched()
			}
			if !joined {
				t.Fatal("follower did not join generation")
			}
			cancel()
			canceled, remaining := leader, follower
			if canceledCaller == "follower" {
				canceled, remaining = follower, leader
			}
			select {
			case got := <-canceled:
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("canceled caller returned %v", got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled caller still waited for shared generation")
			}
			if audits.Load() != 0 {
				t.Fatal("audit ran before shared generation finished")
			}
			unblock()
			select {
			case got := <-remaining:
				if got.err != nil || got.out.Validation.Status != "passed" {
					t.Fatalf("remaining caller failed: %+v %v", got.out, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("remaining caller lost its shared generation")
			}
			if generations.Load() != 1 || audits.Load() != 1 {
				t.Fatalf("generation=%d audit=%d; canceled caller must not validate", generations.Load(), audits.Load())
			}
		})
	}
}

func TestAlreadyCanceledCallsDoNotContactModels(t *testing.T) {
	isolateDomainIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer provider.Close()
	p := Profile{APIKey: "test", BaseURL: provider.URL, Model: "test"}
	if _, err := callLLMContext(ctx, p, "system", "input", 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := doCompileContext(ctx, CompileReq{Text: "text", APIKey: "test", BaseURL: provider.URL}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if result := runBlindValidationContext(ctx, p, p, "text", "text", nil); result.Status != "error" || result.Stages[1].Status != "not_run" {
		t.Fatalf("canceled blind validation proceeded: %+v", result)
	}
	if calls.Load() != 0 {
		t.Fatal("pre-canceled request contacted model")
	}
}

func TestCanceledProxyReleasesProbeWithoutChangingServiceHealth(t *testing.T) {
	const source = "Please review the supplied specification and summarize its operational requirements for the engineering team."
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			compilerCB = newCircuitBreaker(1, 0)
			compilerCB.RecordFailure()
			started, release := make(chan struct{}), make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				if body.Messages[0].Content != verifyGatePrompt {
					writeModelContent(w, "Review the specification; summarize operational requirements for engineers.")
					return
				}
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer provider.Close()
			defer close(release)
			var forwarded atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{"test": {
				APIKey: "test", BaseURL: provider.URL, Model: "test", UpstreamURL: upstream.URL,
			}}}); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"model": "consumer", "max_tokens": 64,
				"messages": []map[string]string{{"role": "user", "content": source}}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(body))).WithContext(ctx)
			response, done := httptest.NewRecorder(), make(chan struct{})
			go func() { newServerMux().ServeHTTP(response, request); close(done) }()
			waitCancellationSignal(t, started, "proxy audit")
			cancel()
			waitCancellationSignal(t, done, "canceled proxy handler")
			available, probeID := compilerCB.Acquire()
			if forwarded.Load() != 0 || compilerCB.Status()["failures"] != 1 || !available {
				t.Fatal("cancellation forwarded upstream, changed service health or left the probe occupied")
			}
			compilerCB.ReleaseProbe(probeID)
			logsMutex.RLock()
			logs := len(proxyLogs)
			logsMutex.RUnlock()
			if logs != 0 {
				t.Fatal("cancellation wrote a misleading compiler failure")
			}
		})
	}
}

func TestCanceledClosedAdmissionCannotReleaseAnotherProbe(t *testing.T) {
	cb := newCircuitBreaker(1, 0)
	allowed, oldAdmission := cb.Acquire()
	if !allowed || oldAdmission != 0 {
		t.Fatal("closed admission incorrectly acquired a probe")
	}
	cb.RecordFailure()
	allowed, firstProbe := cb.Acquire()
	if !allowed || firstProbe == 0 {
		t.Fatal("half-open probe was not acquired")
	}
	cb.ReleaseProbe(oldAdmission)
	if allowed, _ := cb.Acquire(); allowed {
		t.Fatal("canceling an older closed request released another request's probe")
	}
	cb.ReleaseProbe(firstProbe)
	allowed, nextProbe := cb.Acquire()
	if !allowed || nextProbe == firstProbe {
		t.Fatal("new probe did not receive a new identity")
	}
	cb.ReleaseProbe(firstProbe)
	if allowed, _ := cb.Acquire(); allowed {
		t.Fatal("late cancellation released a newer probe")
	}
	cb.Reset()
	cb.RecordFailure()
	allowed, afterReset := cb.Acquire()
	if !allowed || afterReset == nextProbe {
		t.Fatal("reset reused a previous probe identity")
	}
	cb.ReleaseProbe(nextProbe)
	if allowed, _ := cb.Acquire(); allowed {
		t.Fatal("pre-reset cancellation released a post-reset probe")
	}
}
