package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML string

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func bindAvailableListener(preferredAddr string) (net.Listener, string, error) {
	if preferredAddr != "" {
		host, port, err := loopbackListenAddress(preferredAddr)
		if err != nil {
			return nil, preferredAddr, err
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, preferredAddr, err
		}
		return ln, ln.Addr().String(), nil
	}
	for port := 8080; port <= 8099; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if ln, err := net.Listen("tcp", addr); err == nil {
			return ln, addr, nil
		}
	}
	return nil, "", fmt.Errorf("8080-8099 端口均被占用，请通过 -serve 127.0.0.1:<端口> 手动指定")
}

func newServerMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, path := range posterAssetPaths() {
		mux.HandleFunc(path, servePosterAsset)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	mux.HandleFunc("/api/dialects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"detail": "当前固定使用 mixed 高密度巴别语，不再编辑输出预设；旧预设文件保留但不加载，请使用领域词典维护专业表达"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"dialects": listSortedDialects()})
	})
	mux.HandleFunc("/api/domains", domainAPIHandler)
	mux.HandleFunc("/api/validate", candidateValidationHandler)

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			cfg := loadConfig()
			safeProfiles := make(map[string]map[string]any)
			for name, prof := range cfg.Profiles {
				safeProfiles[name] = map[string]any{
					"name":             name,
					"api_key_masked":   maskKey(prof.APIKey),
					"has_api_key":      prof.APIKey != "",
					"base_url":         prof.BaseURL,
					"model":            prof.Model,
					"upstream_url":     prof.UpstreamURL,
					"token_encoding":   normalizeTokenEncoding(prof.TokenEncoding),
					"validation_mode":  normalizeValidationMode(prof.ValidationMode),
					"reader_profile":   prof.ReaderProfile,
					"audit_profile":    prof.AuditProfile,
					"disable_thinking": prof.DisableThinking,
					"reasoning_effort": prof.ReasoningEffort,
				}
			}
			cacheMutex.RLock()
			cacheCount := len(diskCache)
			cacheMutex.RUnlock()

			json.NewEncoder(w).Encode(map[string]any{
				"active_profile":   cfg.ActiveProfile,
				"config_path":      configPath(),
				"cache_path":       cacheFilePath(),
				"cache_count":      cacheCount,
				"profiles":         safeProfiles,
				"token_encodings":  supportedTokenEncodings(),
				"validation_modes": []string{"compare", "blind"},
			})
			return
		}

		if r.Method == "POST" {
			var req struct {
				Action          string  `json:"action"`
				ProfileName     string  `json:"profile_name"`
				APIKey          string  `json:"api_key"`
				BaseURL         string  `json:"base_url"`
				Model           string  `json:"model"`
				UpstreamURL     *string `json:"upstream_url"`
				TokenEncoding   *string `json:"token_encoding"`
				ValidationMode  *string `json:"validation_mode"`
				ReaderProfile   *string `json:"reader_profile"`
				AuditProfile    *string `json:"audit_profile"`
				DisableThinking *bool   `json:"disable_thinking"`
				ReasoningEffort *string `json:"reasoning_effort"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
				return
			}

			// 序列化整个读-改-写：并发保存/切换不同方案时不会互相覆盖。
			configMutationMutex.Lock()
			defer configMutationMutex.Unlock()

			cfg := loadConfig()
			name := strings.TrimSpace(req.ProfileName)
			if name == "" {
				name = cfg.ActiveProfile
			}
			if name == "" {
				name = "default"
			}

			switch req.Action {
			case "delete":
				var readers, auditors []string
				for profileName, profile := range cfg.Profiles {
					if profileName == name {
						continue
					}
					if strings.TrimSpace(profile.ReaderProfile) == name {
						readers = append(readers, profileName)
					}
					if strings.TrimSpace(profile.AuditProfile) == name {
						auditors = append(auditors, profileName)
					}
				}
				if len(readers) > 0 || len(auditors) > 0 {
					var details []string
					if len(readers) > 0 {
						sort.Strings(readers)
						details = append(details, "阅读器: "+strings.Join(readers, ", "))
					}
					if len(auditors) > 0 {
						sort.Strings(auditors)
						details = append(details, "校验方案: "+strings.Join(auditors, ", "))
					}
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"detail": "该方案仍被引用，请先修改这些方案的设置（" + strings.Join(details, "；") + "）"})
					return
				}
				if len(cfg.Profiles) <= 1 {
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(map[string]string{"detail": "至少保留一个配置方案"})
					return
				}
				delete(cfg.Profiles, name)
				if cfg.ActiveProfile == name {
					for k := range cfg.Profiles {
						cfg.ActiveProfile = k
						break
					}
				}
			case "switch":
				if _, ok := cfg.Profiles[name]; !ok {
					w.WriteHeader(404)
					json.NewEncoder(w).Encode(map[string]string{"detail": "配置方案不存在"})
					return
				}
				cfg.ActiveProfile = name
			default:
				existing := cfg.Profiles[name]
				existing.Name = name
				updated, err := applyCompilerCredentials(existing, req.BaseURL, req.APIKey)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
					return
				}
				existing = updated
				if req.Model != "" {
					existing.Model = req.Model
				}
				if req.UpstreamURL != nil {
					existing.UpstreamURL = *req.UpstreamURL
				}
				if req.TokenEncoding != nil {
					existing.TokenEncoding = normalizeTokenEncoding(*req.TokenEncoding)
				}
				if req.ValidationMode != nil {
					existing.ValidationMode = normalizeValidationMode(*req.ValidationMode)
				}
				if req.ReaderProfile != nil {
					existing.ReaderProfile = strings.TrimSpace(*req.ReaderProfile)
				}
				if req.AuditProfile != nil {
					existing.AuditProfile = strings.TrimSpace(*req.AuditProfile)
				}
				if req.DisableThinking != nil {
					existing.DisableThinking = *req.DisableThinking
				}
				if req.ReasoningEffort != nil {
					existing.ReasoningEffort = normalizeReasoningEffort(*req.ReasoningEffort)
				}
				optionErr := validateTokenEncoding(existing.TokenEncoding)
				if optionErr == nil {
					optionErr = validateValidationMode(existing.ValidationMode)
				}
				if optionErr == nil && req.ReasoningEffort != nil {
					optionErr = validateReasoningEffort(*req.ReasoningEffort)
				}
				if optionErr == nil && existing.ReaderProfile != "" {
					if _, exists := cfg.Profiles[existing.ReaderProfile]; !exists {
						optionErr = fmt.Errorf("阅读器配置方案不存在: %s", existing.ReaderProfile)
					}
				}
				if optionErr == nil && existing.AuditProfile != "" {
					if _, exists := cfg.Profiles[existing.AuditProfile]; !exists {
						optionErr = fmt.Errorf("校验配置方案不存在: %s", existing.AuditProfile)
					}
				}
				if optionErr != nil {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"detail": optionErr.Error()})
					return
				}
				cfg.Profiles[name] = existing
				cfg.ActiveProfile = name
			}

			if err := writeConfigFile(cfg); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"detail": "写入配置文件失败: " + err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "config_path": configPath()})
		}
	})

	mux.HandleFunc("/api/config/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(405)
			json.NewEncoder(w).Encode(map[string]string{"detail": "仅支持 POST"})
			return
		}

		var req struct {
			ProfileName string `json:"profile_name"`
			APIKey      string `json:"api_key"`
			BaseURL     string `json:"base_url"`
			Model       string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
			return
		}

		cfg := loadConfig()
		name := strings.TrimSpace(req.ProfileName)
		var prof Profile
		if name == "" {
			prof = getActiveProfile(cfg, "")
		} else {
			// An unsaved draft has no inherited credentials. Form values below
			// can still test it before saving, without borrowing another account.
			prof = cfg.Profiles[name]
			prof.Name = name
		}

		// 表单值优先于已存配置：允许在保存前先验证一份新的 Key / 地址 / 模型。
		updated, err := applyCompilerCredentials(prof, req.BaseURL, req.APIKey)
		if err != nil {
			json.NewEncoder(w).Encode(ProbeResult{OK: false, Detail: err.Error(),
				Hint: "更换请求地址时，请重新输入该地址对应的 API Key"})
			return
		}
		prof = updated
		if model := strings.TrimSpace(req.Model); model != "" {
			prof.Model = model
		}

		json.NewEncoder(w).Encode(probeProfile(prof))
	})

	mux.HandleFunc("/api/compile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"detail": "仅支持 POST"})
			return
		}
		var req CompileReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"detail": "无效JSON"})
			return
		}
		res, err := doCompileContext(r.Context(), req)
		if err != nil {
			status := 400
			body := map[string]any{"detail": err.Error()}
			if res.Compiled != "" {
				// A rejected draft is useful for inspection. HTTP status and the
				// failed/error validation remain authoritative for adoption.
				body["compiled"], body["dialect"] = res.Compiled, res.Dialect
				body["cache_hit"], body["stats"] = res.CacheHit, res.Stats
				body["validation"] = newValidation("error", "none", err.Error())
				status = http.StatusBadGateway
			}
			var validationErr *ValidationError
			if errors.As(err, &validationErr) {
				body["validation"] = validationErr.Validation
				if validationErr.Validation.Status == "error" {
					status = http.StatusBadGateway
				} else {
					status = http.StatusUnprocessableEntity
				}
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(body)
			return
		}
		json.NewEncoder(w).Encode(res)
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			logsMutex.Lock()
			proxyLogs = nil
			logsMutex.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
			return
		}

		if r.Method == "POST" {
			var req struct {
				Action  string `json:"action"` // toggle_redact | reset_circuit
				Enabled bool   `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				switch req.Action {
				case "toggle_redact":
					setLogRedactEnabled(req.Enabled)
				case "reset_circuit":
					compilerCB.Reset()
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status":         "ok",
				"redact_enabled": isLogRedactEnabled(),
				"circuit":        compilerCB.Status(),
			})
			return
		}

		logsMutex.RLock()
		list := make([]ProxyLog, len(proxyLogs))
		copy(list, proxyLogs)
		logsMutex.RUnlock()

		json.NewEncoder(w).Encode(map[string]any{
			"redact_enabled": isLogRedactEnabled(),
			"circuit":        compilerCB.Status(),
			"concurrency":    proxyCompileSem.Status(),
			"logs":           list,
		})
	})

	mux.HandleFunc("/api/compile/stream", compileStreamHandler)
	mux.HandleFunc("/v1/chat/completions", openAIProxyHandler)
	mux.HandleFunc("/v1/messages", anthropicProxyHandler)

	return mux
}

func startServer(preferredAddr string, autoOpen bool) {
	ln, actualAddr, err := bindAvailableListener(preferredAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 启动失败: %v\n", err)
		os.Exit(1)
	}
	handler, err := newServerHandler(actualAddr)
	if err != nil {
		_ = ln.Close()
		fmt.Fprintf(os.Stderr, "[Error] 启动失败: %v\n", err)
		os.Exit(1)
	}

	// 服务端模式才启用代理提速默认值；显式 =0 / =false 会覆盖这里的默认。
	configureProxySpeedDefaults()

	// 服务端模式才启用异步落盘；退出前会强制刷盘，不丢缓存。
	stopCacheFlusher := startAsyncCacheFlusher(500 * time.Millisecond)
	defer stopCacheFlusher()

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB；WriteTimeout 留空以支持长 SSE 流。
	}

	webURL := "http://" + actualAddr
	fmt.Printf("[+] Babel-DSL 服务已就绪:\n")
	fmt.Printf("    - 配置文件:       %s\n", configPath())
	fmt.Printf("    - 表达策略:       mixed（高密度巴别语 / 关系·条件·约束）\n")
	fmt.Printf("    - 领域表达目录:   %s (支持 ./domains 覆盖)\n", globalDomainsDir())
	fmt.Printf("    - LRU 缓存池:     %s (上限 %d 条，异步防抖落盘)\n", cacheFilePath(), maxCacheEntries)
	fmt.Printf("    - 代理并发控制:   全局上限 %d / 单请求块并发 %d / 队列超时 %v\n",
		proxyCompileSem.Status()["limit"], proxyBlockConcurrency, proxyCompileQueueTimeout)
	fmt.Printf("    - 代理提速:       校验缓存 %s / 负缓存 %s / 异步预热 %t / 最小长度 %d / 跳过非散文 %t\n",
		proxySpeedDurationLabel(proxyValidationCacheTTL), proxySpeedDurationLabel(proxyNegativeCacheTTL),
		proxyAsyncWarmup, proxyMinRunes, proxySkipNonProse)
	fmt.Printf("    - Web 控制台:     %s\n", webURL)
	fmt.Printf("    - OpenAI 代理:    %s/v1/chat/completions\n", webURL)
	fmt.Printf("    - Anthropic 代理: %s/v1/messages\n", webURL)

	if autoOpen {
		openBrowser(webURL)
	}

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[Error] 服务异常退出: %v\n", err)
			os.Exit(1)
		}
	case <-sigCtx.Done():
		// 停止接收新连接，给在途请求（含 SSE）15s 收尾时间，再强制刷缓存。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh
	}
}
