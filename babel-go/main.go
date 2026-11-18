package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML string

type Config struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

type CompileReq struct {
	Text    string `json:"text"`
	Mode    string `json:"mode"`
	Verify  bool   `json:"verify"`
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
}

type CompileResp struct {
	Compiled string `json:"compiled"`
	Audit    string `json:"audit,omitempty"`
	Stats    struct {
		OriginalChars    int    `json:"original_chars"`
		CompiledChars    int    `json:"compiled_chars"`
		CompressionRatio string `json:"compression_ratio"`
	} `json:"stats"`
}

var prompts = map[string]string{
	"standard": `你是一个「高密度提示词编译器」。将自然语言无损编译为高信息密度的结构化符号语言。
规则：1.零信息丢失，保留所有技术参数、Payload、边界与约束。2.剔除废话与连接词。3.符号规范：因果/流向用 →；并列用 +；枚举/或用 |；否定用 ¬；遍历用 ∀；存在/命中用 ∃；变更用 ↔。4.使用 ASCII 树形线 (├ │ ╰) 严格划分层级。5.仅输出编译后的纯文本，不要任何解释或 Markdown 代码块包裹。`,
	"extreme": `你是一个「极限数理逻辑编译器（Babel-DSL）」。将自然语言压缩为最高密度的数理逻辑与符号表达式。
规则：1.强制使用符号替代逻辑词：定义:= | 因果→ | 与∧ | 或| | 非¬ | 遍历∀ | 命中∃ | 属于∈ | 篡改↔ | 探测? | 红线⚠。2.使用 ├ │ ╰ 构建紧凑缩进树。3.保留所有专有名词、代码片段、Payload 与数字阈值。4.仅输出编译后的纯文本，不要任何解释或 Markdown 代码块包裹。`,
}

const verifyPrompt = `你是一个语义无损审计器。对比【原文】与【编译后DSL】，检查是否存在技术细节、边界条件、Payload或约束要求的遗漏。若完全无损，仅输出：[PASS] 语义100%无损；若有遗漏，简明列出缺失点。`

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".babel_config.json")
}

func loadConfig() Config {
	cfg := Config{
		APIKey:  os.Getenv("BABEL_API_KEY"),
		BaseURL: os.Getenv("BABEL_BASE_URL"),
		Model:   os.Getenv("BABEL_MODEL"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if data, err := os.ReadFile(configPath()); err == nil {
		var saved Config
		if json.Unmarshal(data, &saved) == nil {
			if saved.APIKey != "" {
				cfg.APIKey = saved.APIKey
			}
			if saved.BaseURL != "" {
				cfg.BaseURL = saved.BaseURL
			}
			if saved.Model != "" {
				cfg.Model = saved.Model
			}
		}
	}
	return cfg
}

func saveConfig(cfg Config) error {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(configPath(), data, 0600)
}

func callLLM(cfg Config, sysPrompt, userPrompt string, temp float64) (string, error) {
	if cfg.APIKey == "" {
		return "", fmt.Errorf("未配置 API Key，请设置 BABEL_API_KEY 或通过前端/CLI 保存配置")
	}
	reqBody := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": temp,
	}
	jsonBytes, _ := json.Marshal(reqBody)
	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API 错误 (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil || len(result.Choices) == 0 {
		return "", fmt.Errorf("解析响应失败: %s", string(bodyBytes))
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func doCompile(req CompileReq) (CompileResp, error) {
	cfg := loadConfig()
	if req.APIKey != "" {
		cfg.APIKey = req.APIKey
	}
	if req.BaseURL != "" {
		cfg.BaseURL = req.BaseURL
	}
	if req.Model != "" {
		cfg.Model = req.Model
	}
	if req.Mode == "" {
		req.Mode = "extreme"
	}
	sys, ok := prompts[req.Mode]
	if !ok {
		sys = prompts["extreme"]
	}

	compiled, err := callLLM(cfg, sys, req.Text, 0.1)
	if err != nil {
		return CompileResp{}, err
	}

	var out CompileResp
	out.Compiled = compiled
	if req.Verify {
		audit, err := callLLM(cfg, verifyPrompt, fmt.Sprintf("【原文】:\n%s\n\n【编译后DSL】:\n%s", req.Text, compiled), 0.0)
		if err == nil {
			out.Audit = audit
		}
	}

	origRunes := len([]rune(req.Text))
	compRunes := len([]rune(compiled))
	ratio := 0.0
	if origRunes > 0 {
		ratio = (1.0 - float64(compRunes)/float64(origRunes)) * 100
	}
	out.Stats.OriginalChars = origRunes
	out.Stats.CompiledChars = compRunes
	out.Stats.CompressionRatio = fmt.Sprintf("%.1f%%", ratio)
	return out, nil
}

func startServer(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			cfg := loadConfig()
			masked := ""
			if len(cfg.APIKey) > 6 {
				masked = cfg.APIKey[:6] + "***"
			}
			json.NewEncoder(w).Encode(map[string]string{
				"api_key_masked": masked,
				"base_url":       cfg.BaseURL,
				"model":          cfg.Model,
			})
			return
		}
		if r.Method == "POST" {
			var cfg Config
			json.NewDecoder(r.Body).Decode(&cfg)
			saveConfig(cfg)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	})
	http.HandleFunc("/api/compile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CompileReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"detail":"无效JSON"}`, 400)
			return
		}
		res, err := doCompile(req)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(res)
	})
	fmt.Printf("[+] Babel-DSL 服务已启动: http://%s\n", addr)
	http.ListenAndServe(addr, nil)
}

func main() {
	serveAddr := flag.String("serve", "", "启动 Web/API 服务地址 (例如 :8080)")
	mode := flag.String("m", "extreme", "压缩模式: standard | extreme")
	verify := flag.Bool("v", false, "开启双向语义无损校验")
	quiet := flag.Bool("q", false, "静默模式，仅输出纯文本")
	file := flag.String("f", "", "从文件读取输入")
	apiKey := flag.String("api-key", "", "指定 API Key")
	baseURL := flag.String("base-url", "", "指定 Base URL")
	model := flag.String("model", "", "指定模型名称")
	saveCfg := flag.Bool("save-config", false, "保存 API 配置到本地")
	flag.Parse()

	if *saveCfg {
		cfg := loadConfig()
		if *apiKey != "" {
			cfg.APIKey = *apiKey
		}
		if *baseURL != "" {
			cfg.BaseURL = *baseURL
		}
		if *model != "" {
			cfg.Model = *model
		}
		saveConfig(cfg)
		if !*quiet {
			fmt.Fprintln(os.Stderr, "[+] 配置已保存至 ~/.babel_config.json")
		}
		if *serveAddr == "" && *file == "" && flag.NArg() == 0 {
			return
		}
	}

	if *serveAddr != "" {
		startServer(*serveAddr)
		return
	}

	// 读取输入源
	var input string
	if flag.NArg() > 0 {
		input = strings.Join(flag.Args(), " ")
	} else if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Error] 读取文件失败: %v\n", err)
			os.Exit(1)
		}
		input = string(b)
	} else {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			input = string(b)
		} else {
			// 无参数且无管道输入时，默认启动本地 Web 服务
			startServer("127.0.0.1:8080")
			return
		}
	}

	res, err := doCompile(CompileReq{
		Text:    strings.TrimSpace(input),
		Mode:    *mode,
		Verify:  *verify,
		APIKey:  *apiKey,
		BaseURL: *baseURL,
		Model:   *model,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] %v\n", err)
		os.Exit(1)
	}

	fmt.Println(res.Compiled)
	if !*quiet {
		fmt.Fprintf(os.Stderr, "\n--- 统计: %d → %d 字符 (压缩率 %s) ---\n", res.Stats.OriginalChars, res.Stats.CompiledChars, res.Stats.CompressionRatio)
		if res.Audit != "" {
			fmt.Fprintf(os.Stderr, "--- 校验: %s ---\n", res.Audit)
		}
	}
}