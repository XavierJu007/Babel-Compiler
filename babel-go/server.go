package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		return ln, preferredAddr, err
	}
	for port := 8080; port <= 8099; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if ln, err := net.Listen("tcp", addr); err == nil {
			return ln, addr, nil
		}
	}
	return nil, "", fmt.Errorf("8080-8099 端口均被占用，请通过 -serve 127.0.0.1:<端口> 手动指定")
}

func startServer(preferredAddr string, autoOpen bool) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	http.HandleFunc("/api/dialects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			reloadDialects()
			json.NewEncoder(w).Encode(map[string]any{
				"global_dir":    globalDialectsDir(),
				"workspace_dir": workspaceDialectsDir(),
				"dialects":      listSortedDialects(),
			})
			return
		}

		if r.Method == "POST" {
			var req struct {
				Action string `json:"action"` // save | delete | reload | reset_defaults
				Name   string `json:"name"`
				Label  string `json:"label"`
				Prompt string `json:"prompt"`
				Scope  string `json:"scope"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
				return
			}

			switch req.Action {
			case "reload":
				cnt := reloadDialects()
				json.NewEncoder(w).Encode(map[string]any{"status": "reloaded", "count": cnt})
				return
			case "reset_defaults":
				writeBuiltinDialects(globalDialectsDir(), true)
				cnt := reloadDialects()
				json.NewEncoder(w).Encode(map[string]any{"status": "reset", "count": cnt})
				return
			}

			name := sanitizeDialectName(req.Name)
			if name == "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "方言名称仅支持字母、数字、下划线与连字符"})
				return
			}

			targetDir := globalDialectsDir()
			if req.Scope == "workspace" {
				targetDir = workspaceDialectsDir()
			}
			_ = os.MkdirAll(targetDir, 0755)
			filePath := filepath.Join(targetDir, name+".md")

			if req.Action == "delete" {
				_ = os.Remove(filepath.Join(workspaceDialectsDir(), name+".md"))
				_ = os.Remove(filepath.Join(globalDialectsDir(), name+".md"))
				reloadDialects()
				json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
				return
			}

			label := strings.TrimSpace(req.Label)
			if label == "" {
				label = name
			}
			prompt := strings.TrimSpace(req.Prompt)
			if prompt == "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "方言提示词规则不能为空"})
				return
			}

			mdContent := fmt.Sprintf("# Label: %s\n%s\n", label, prompt)
			if err := os.WriteFile(filePath, []byte(mdContent), 0644); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"detail": "写入方言文件失败: " + err.Error()})
				return
			}
			reloadDialects()
			json.NewEncoder(w).Encode(map[string]string{"status": "saved", "file_path": filePath})
		}
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			cfg := loadConfig()
			safeProfiles := make(map[string]map[string]any)
			for name, prof := range cfg.Profiles {
				safeProfiles[name] = map[string]any{
					"name":           name,
					"api_key_masked": maskKey(prof.APIKey),
					"has_api_key":    prof.APIKey != "",
					"base_url":       prof.BaseURL,
					"model":          prof.Model,
					"upstream_url":   prof.UpstreamURL,
				}
			}
			cacheMutex.RLock()
			cacheCount := len(diskCache)
			cacheMutex.RUnlock()

			json.NewEncoder(w).Encode(map[string]any{
				"active_profile": cfg.ActiveProfile,
				"config_path":    configPath(),
				"cache_path":     cacheFilePath(),
				"cache_count":    cacheCount,
				"profiles":       safeProfiles,
			})
			return
		}

		if r.Method == "POST" {
			var req struct {
				Action      string `json:"action"`
				ProfileName string `json:"profile_name"`
				APIKey      string `json:"api_key"`
				BaseURL     string `json:"base_url"`
				Model       string `json:"model"`
				UpstreamURL string `json:"upstream_url"`
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
				existing.UpstreamURL = req.UpstreamURL
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

	http.HandleFunc("/api/config/test", func(w http.ResponseWriter, r *http.Request) {
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
		prof := getActiveProfile(cfg, strings.TrimSpace(req.ProfileName))
		prof.Name = firstNonEmpty(strings.TrimSpace(req.ProfileName), cfg.ActiveProfile, prof.Name, "default")

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

	http.HandleFunc("/api/compile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CompileReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"detail": "无效JSON"})
			return
		}
		res, err := doCompile(req)
		if err != nil {
			status := 400
			if strings.Contains(err.Error(), "门禁拦截") {
				status = 422
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/v1/chat/completions", openAIProxyHandler)
	http.HandleFunc("/v1/messages", anthropicProxyHandler)

	ln, actualAddr, err := bindAvailableListener(preferredAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 启动失败: %v\n", err)
		os.Exit(1)
	}

	webURL := "http://" + actualAddr
	fmt.Printf("[+] Babel-DSL 服务已就绪:\n")
	fmt.Printf("    - 配置文件:       %s\n", configPath())
	fmt.Printf("    - 方言词典目录:   %s (支持 ./dialects 覆盖)\n", globalDialectsDir())
	fmt.Printf("    - LRU 缓存池:     %s (上限 %d 条)\n", cacheFilePath(), maxCacheEntries)
	fmt.Printf("    - Web 控制台:     %s\n", webURL)
	fmt.Printf("    - OpenAI 代理:    %s/v1/chat/completions\n", webURL)
	fmt.Printf("    - Anthropic 代理: %s/v1/messages\n", webURL)

	if autoOpen {
		openBrowser(webURL)
	}

	if err := http.Serve(ln, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 服务异常退出: %v\n", err)
		os.Exit(1)
	}
}
