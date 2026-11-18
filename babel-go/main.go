package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	initDiskCache()
	reloadDialects()

	serveAddr := flag.String("serve", "", "指定 Web/API/代理服务地址 (留空则自动探测可用端口)")
	profileName := flag.String("profile", "", "指定使用的已保存配置方案名称")
	mode := flag.String("m", "extreme", "指定方言名称 (如 extreme | token | standard | code-audit | 自定义.md名称)")
	listDialects := flag.Bool("list-dialects", false, "列出所有已加载的方言词典并退出")
	verify := flag.Bool("v", false, "开启往返语义审计")
	strictGate := flag.Bool("gate", false, "开启硬门禁：缺失数值、阈值或否定约束时直接报错失败")
	noCache := flag.Bool("no-cache", false, "跳过本地确定性缓存强制重新编译")
	quiet := flag.Bool("q", false, "静默模式，仅输出纯文本")
	file := flag.String("f", "", "从文件读取输入")
	apiKey := flag.String("api-key", "", "指定 API Key")
	baseURL := flag.String("base-url", "", "指定编译器 Base URL")
	model := flag.String("model", "", "指定编译器模型名称")
	saveCfg := flag.Bool("save-config", false, "保存当前参数为配置方案")
	flag.Parse()

	if *listDialects {
		for _, d := range listSortedDialects() {
			fmt.Printf("%-14s | %-28s | [%s] %s\n", d.Name, d.Label, d.Source, d.FilePath)
		}
		return
	}

	if *saveCfg {
		cfg := loadConfig()
		name := *profileName
		if name == "" {
			name = cfg.ActiveProfile
		}
		p := cfg.Profiles[name]
		p.Name = name
		if *apiKey != "" {
			p.APIKey = *apiKey
		}
		if *baseURL != "" {
			p.BaseURL = *baseURL
		}
		if *model != "" {
			p.Model = *model
		}
		cfg.Profiles[name] = p
		cfg.ActiveProfile = name
		if err := writeConfigFile(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[Error] 保存失败: %v\n", err)
			os.Exit(1)
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[+] 配置方案 [%s] 已保存至 %s\n", name, configPath())
		}
		if *serveAddr == "" && *file == "" && flag.NArg() == 0 {
			return
		}
	}

	if *serveAddr != "" {
		startServer(*serveAddr, false)
		return
	}

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
			startServer("", true)
			return
		}
	}

	res, err := doCompile(CompileReq{
		Text:       strings.TrimSpace(input),
		Mode:       *mode,
		Verify:     *verify,
		StrictGate: *strictGate,
		NoCache:    *noCache,
		Profile:    *profileName,
		APIKey:     *apiKey,
		BaseURL:    *baseURL,
		Model:      *model,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] %v\n", err)
		os.Exit(1)
	}

	fmt.Println(res.Compiled)
	if !*quiet {
		cacheTag := "MISS"
		if res.CacheHit {
			cacheTag = "HIT"
		}
		fmt.Fprintf(os.Stderr, "\n--- 计量 [%s]: %s [Cache: %s] ---\n", res.Dialect, res.Stats.CompressionRatio, cacheTag)
		if res.Audit != "" {
			fmt.Fprintf(os.Stderr, "--- 门禁: %s ---\n", res.Audit)
		}
	}
}
