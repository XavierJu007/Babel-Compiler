package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func configPath() string {
	localCfg := "babel.config.json"
	if _, err := os.Stat(localCfg); err == nil {
		return localCfg
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return localCfg
	}
	return filepath.Join(home, ".babel_config.json")
}

func defaultProfile() Profile {
	base := os.Getenv("BABEL_BASE_URL")
	if base == "" {
		base = defaultCompilerRequestURL
	}
	model := os.Getenv("BABEL_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return Profile{
		Name:           "default",
		APIKey:         os.Getenv("BABEL_API_KEY"),
		BaseURL:        base,
		Model:          model,
		UpstreamURL:    os.Getenv("BABEL_UPSTREAM_URL"),
		TokenEncoding:  os.Getenv("BABEL_TOKEN_ENCODING"),
		ValidationMode: os.Getenv("BABEL_VALIDATION_MODE"),
	}
}

func loadConfig() Config {
	cfg := Config{
		ActiveProfile: "default",
		Profiles:      map[string]Profile{"default": defaultProfile()},
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved Config
	if json.Unmarshal(data, &saved) != nil {
		return cfg
	}
	if saved.Profiles == nil {
		saved.Profiles = make(map[string]Profile)
	}
	if len(saved.Profiles) == 0 && (saved.APIKey != "" || saved.BaseURL != "" || saved.Model != "") {
		p := defaultProfile()
		if saved.APIKey != "" {
			p.APIKey = saved.APIKey
		}
		if saved.BaseURL != "" {
			p.BaseURL = saved.BaseURL
		}
		if saved.Model != "" {
			p.Model = saved.Model
		}
		if saved.UpstreamURL != "" {
			p.UpstreamURL = saved.UpstreamURL
		}
		saved.Profiles["default"] = p
	}
	if len(saved.Profiles) > 0 {
		cfg.Profiles = saved.Profiles
	}
	if saved.ActiveProfile != "" {
		if _, exists := cfg.Profiles[saved.ActiveProfile]; exists {
			cfg.ActiveProfile = saved.ActiveProfile
		}
	}
	return cfg
}

func getActiveProfile(cfg Config, overrideName string) Profile {
	name := cfg.ActiveProfile
	if overrideName != "" {
		name = overrideName
	}
	if p, ok := cfg.Profiles[name]; ok {
		p.Name = name // The map key is the identity, including legacy files without name.
		return p
	}
	for name, p := range cfg.Profiles {
		p.Name = name
		return p
	}
	return defaultProfile()
}

func writeConfigFile(cfg Config) error {
	active := getActiveProfile(cfg, "")
	cfg.APIKey = active.APIKey
	cfg.BaseURL = active.BaseURL
	cfg.Model = active.Model
	cfg.UpstreamURL = active.UpstreamURL
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func maskKey(k string) string {
	if len(k) <= 6 {
		if k == "" {
			return ""
		}
		return "***"
	}
	return k[:6] + "***"
}

// ==================== 模型连通性探测 ====================

// probeTimeout 短于编译主链路（60s），保证「测试连接」不会让用户干等，
// 同时给推理型模型留出足够首响时间。
const probeTimeout = 30 * time.Second

// ProbeResult 是「测试连接」的结构化结果，永远不包含明文 API Key。
type ProbeResult struct {
	OK            bool   `json:"ok"`
	Detail        string `json:"detail"`
	Hint          string `json:"hint,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	StatusCode    int    `json:"status_code,omitempty"`
	TargetURL     string `json:"target_url"`
	Model         string `json:"model"`
	ProviderModel string `json:"provider_model,omitempty"`
}

// probeProfile 向配置的请求 URL 发送一次最小 OpenAI 兼容请求。
// 与 callLLM 共享精确地址规则；试连成功只说明本次最小请求成功。
func probeProfile(p Profile) ProbeResult {
	p = effectiveCompilerProfile(p)
	model := strings.TrimSpace(p.Model)
	res := ProbeResult{Model: model, TargetURL: compilerRequestURL(p.BaseURL)}

	if strings.TrimSpace(p.APIKey) == "" {
		res.Detail = fmt.Sprintf("配置方案 [%s] 未设置 API Key", p.Name)
		res.Hint = "在弹窗填入 API Key 后可直接测试，无需先保存"
		return res
	}

	body, err := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens":  1,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		res.Detail = "构造探测请求失败: " + err.Error()
		return res
	}

	req, err := http.NewRequest("POST", res.TargetURL, bytes.NewReader(body))
	if err != nil {
		res.Detail = "目标 URL 无效: " + err.Error()
		res.Hint = "填写服务商实际接收 POST 的 http(s) 请求 URL；程序不自动追加路径"
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := &http.Client{Timeout: probeTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Detail = "无法建立连接: " + err.Error()
		res.Hint = probeNetworkHint(err)
		return res
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	res.StatusCode = resp.StatusCode

	if resp.StatusCode != 200 {
		res.Detail = fmt.Sprintf("上游返回 HTTP %d: %s", resp.StatusCode, extractUpstreamError(raw))
		res.Hint = probeStatusHint(resp.StatusCode)
		return res
	}

	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		res.Detail = "端点可达，但响应不是 OpenAI 兼容格式: " + truncateRunes(string(raw), 200)
		res.Hint = "确认填写的请求 URL 接收 OpenAI 兼容请求，并返回 choices/message 格式；程序不自动追加路径"
		return res
	}

	res.OK = true
	res.ProviderModel = parsed.Model
	res.Detail = fmt.Sprintf("连接成功，模型 %s 已响应", firstNonEmpty(parsed.Model, model))
	return res
}

func probeStatusHint(code int) string {
	switch {
	case code == 401 || code == 403:
		return "API Key 缺失、无效，或无权访问该模型"
	case code == 404:
		return "请求 URL 或模型名不存在：核对服务商实际接收 POST 的地址，程序不自动追加路径"
	case code == 400:
		return "请求被拒绝：常见原因是模型名不被支持，或该端点不兼容 max_tokens 参数"
	case code == 429:
		return "触发上游限流或额度不足，稍后重试"
	case code >= 500:
		return "上游服务异常，稍后重试或更换服务商"
	}
	return ""
}

func probeNetworkHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "连接超时：检查网络、代理设置，或确认请求 URL 的主机可达"
	case strings.Contains(msg, "no such host"):
		return "域名解析失败：检查请求 URL 是否拼写错误"
	case strings.Contains(msg, "connection refused"):
		return "端口拒绝连接：本地 Ollama 等服务需先启动"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls"):
		return "TLS 证书校验失败：确认协议与证书是否有效"
	case strings.Contains(msg, "unsupported protocol scheme"):
		return "请求 URL 需以 http:// 或 https:// 开头"
	}
	return "检查请求 URL、网络连通性与代理设置"
}

func extractUpstreamError(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Error.Message != "" {
			return truncateRunes(e.Error.Message, 300)
		}
		if e.Message != "" {
			return truncateRunes(e.Message, 300)
		}
	}
	return truncateRunes(strings.TrimSpace(string(raw)), 300)
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
