package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
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
		ln, err := net.Listen("tcp", preferredAddr)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
					"name":            name,
					"api_key_masked":  maskKey(prof.APIKey),
					"has_api_key":     prof.APIKey != "",
					"base_url":        prof.BaseURL,
					"model":           prof.Model,
					"upstream_url":    prof.UpstreamURL,
					"token_encoding":  normalizeTokenEncoding(prof.TokenEncoding),
					"validation_mode": normalizeValidationMode(prof.ValidationMode),
					"reader_profile":  prof.ReaderProfile,
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
				Action         string  `json:"action"`
				ProfileName    string  `json:"profile_name"`
				APIKey         string  `json:"api_key"`
				BaseURL        string  `json:"base_url"`
				Model          string  `json:"model"`
				UpstreamURL    *string `json:"upstream_url"`
				TokenEncoding  *string `json:"token_encoding"`
				ValidationMode *string `json:"validation_mode"`
				ReaderProfile  *string `json:"reader_profile"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
				return
			}

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
				var readers []string
				for profileName, profile := range cfg.Profiles {
					if profileName != name && strings.TrimSpace(profile.ReaderProfile) == name {
						readers = append(readers, profileName)
					}
				}
				if len(readers) > 0 {
					sort.Strings(readers)
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"detail": "该方案仍被用作阅读器，请先修改这些方案的阅读器选择: " + strings.Join(readers, ", ")})
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
				if req.APIKey != "" && !strings.Contains(req.APIKey, "***") {
					existing.APIKey = req.APIKey
				}
				if req.BaseURL != "" {
					existing.BaseURL = req.BaseURL
				}
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
				optionErr := validateTokenEncoding(existing.TokenEncoding)
				if optionErr == nil {
					optionErr = validateValidationMode(existing.ValidationMode)
				}
				if optionErr == nil && existing.ReaderProfile != "" {
					if _, exists := cfg.Profiles[existing.ReaderProfile]; !exists {
						optionErr = fmt.Errorf("阅读器配置方案不存在: %s", existing.ReaderProfile)
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
		if key := strings.TrimSpace(req.APIKey); key != "" && !strings.Contains(key, "***") {
			prof.APIKey = key
		}
		if base := strings.TrimSpace(req.BaseURL); base != "" {
			prof.BaseURL = base
		}
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
			"logs":           list,
		})
	})

	mux.HandleFunc("/v1/chat/completions", openAIProxyHandler)
	mux.HandleFunc("/v1/messages", anthropicProxyHandler)

	return mux
}

func startServer(preferredAddr string, autoOpen bool) {
	mux := newServerMux()
	ln, actualAddr, err := bindAvailableListener(preferredAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 启动失败: %v\n", err)
		os.Exit(1)
	}

	webURL := "http://" + actualAddr
	fmt.Printf("[+] Babel-DSL 服务已就绪:\n")
	fmt.Printf("    - 配置文件:       %s\n", configPath())
	fmt.Printf("    - 表达策略:       mixed（高密度巴别语 / 关系·条件·约束）\n")
	fmt.Printf("    - 领域表达目录:   %s (支持 ./domains 覆盖)\n", globalDomainsDir())
	fmt.Printf("    - LRU 缓存池:     %s (上限 %d 条)\n", cacheFilePath(), maxCacheEntries)
	fmt.Printf("    - Web 控制台:     %s\n", webURL)
	fmt.Printf("    - OpenAI 代理:    %s/v1/chat/completions\n", webURL)
	fmt.Printf("    - Anthropic 代理: %s/v1/messages\n", webURL)

	if autoOpen {
		openBrowser(webURL)
	}

	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 服务异常退出: %v\n", err)
		os.Exit(1)
	}
}
