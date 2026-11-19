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
	_, domainLoadErr := reloadDomains()

	serveAddr := flag.String("serve", "", "指定 Web/API/代理服务地址 (留空则自动探测可用端口)")
	profileName := flag.String("profile", "", "指定使用的已保存配置方案名称")
	mode := flag.String("m", defaultDialectName, "已弃用，仅兼容旧内置名称；当前固定使用混合表达，无需指定")
	domains := flag.String("domains", "", "组合领域名称，以逗号分隔 (如 application-security,api-contract)")
	listDialects := flag.Bool("list-dialects", false, "已弃用，显示唯一固定表达策略并退出")
	listDomains := flag.Bool("list-domains", false, "列出所有已加载的领域表达库并退出")
	verify := flag.Bool("v", false, "开启原文与候选的语义对照审计")
	strictGate := flag.Bool("gate", false, "严格检查：无法通过本次审计时返回错误")
	noCache := flag.Bool("no-cache", false, "跳过缓存读写，强制重新编译")
	quiet := flag.Bool("q", false, "静默模式，仅输出纯文本")
	file := flag.String("f", "", "从文件读取输入")
	apiKey := flag.String("api-key", "", "指定 API Key")
	baseURL := flag.String("base-url", "", "指定编译/校验请求 URL，按原样发送 POST，不自动追加路径")
	model := flag.String("model", "", "指定编译器模型名称")
	tokenEncoding := flag.String("token-encoding", "", "消费文本计数编码：estimate | cl100k_base | o200k_base（留空继承方案）")
	validationMode := flag.String("validation-mode", "", "验证模式：compare 原文对照 | blind 独立阅读后对照（配合 -v/-gate）")
	readerName := flag.String("reader-profile", "", "blind 阅读器的已保存方案（显式空值使用编译方案）")
	saveCfg := flag.Bool("save-config", false, "保存当前参数为配置方案")
	flag.Parse()
	var readerOverride *string
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "reader-profile" {
			readerOverride = readerName
		}
	})
	if domainLoadErr != nil && !*quiet {
		fmt.Fprintf(os.Stderr, "[领域加载] %v\n", domainLoadErr)
	}
	if *listDomains {
		for _, d := range listSortedDomains() {
			fmt.Printf("%-22s | %-20s | %d 词条 | [%s] %s\n", d.Name, d.Label, len(d.Entries), d.Source, d.FilePath)
		}
		return
	}

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
		if *tokenEncoding != "" {
			p.TokenEncoding = normalizeTokenEncoding(*tokenEncoding)
		}
		if *validationMode != "" {
			p.ValidationMode = normalizeValidationMode(*validationMode)
		}
		if readerOverride != nil {
			p.ReaderProfile = strings.TrimSpace(*readerOverride)
		}
		if err := validateTokenEncoding(p.TokenEncoding); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := validateValidationMode(p.ValidationMode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if p.ReaderProfile != "" {
			if _, exists := cfg.Profiles[p.ReaderProfile]; !exists {
				fmt.Fprintf(os.Stderr, "阅读器配置方案不存在: %s\n", p.ReaderProfile)
				os.Exit(1)
			}
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

	var domainNames []string
	if strings.TrimSpace(*domains) != "" {
		domainNames = strings.Split(*domains, ",")
	}
	res, err := doCompile(CompileReq{
		Text:           input,
		Mode:           *mode,
		Domains:        domainNames,
		Verify:         *verify,
		StrictGate:     *strictGate,
		NoCache:        *noCache,
		Profile:        *profileName,
		APIKey:         *apiKey,
		BaseURL:        *baseURL,
		Model:          *model,
		TokenEncoding:  *tokenEncoding,
		ValidationMode: *validationMode,
		ReaderProfile:  readerOverride,
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
