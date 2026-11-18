package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyStandardPromptFixture = `你是一个「高密度提示词编译器」。将自然语言无损编译为结构清晰的树状符号指令（目标长度为原文的 55%~68%）。
规范：
1. 零私有码本：保留主语与所有技术实体，严禁使用未定义代号，严禁自己编造多余小标题。
2. 算子映射：因为→∵ | 所以/导致→⇒ | 并且→∧ | 或者→| | 不/禁止→¬ | 属于→∈ | 处于状态→@ | 遍历→∀ | 变更→↔。
3. 使用单字符 ASCII 树形线（├ │ ╰）划分层级，每个独立检查项独占一行。
4. 自动修正技术术语中的明显 OCR 错别字（如 userld→userId, bp//filter→php://filter, CRLE→CRLF）。
5. 零空行，仅输出纯文本，不要 Markdown 代码块包裹。`

func TestLegacyGeneratedPresetDoesNotShadowCurrentBuiltin(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	path := filepath.Join(globalDir, "standard.md")
	content := "# Label: old\n" + legacyStandardPromptFixture + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	reloadDialectsFromDirs(globalDir, workspaceDir)
	current := getDialect("standard")
	if current.Source != "builtin" || current.Prompt != builtinDialectTemplates["standard"].Prompt {
		t.Fatal("unchanged legacy generated template still shadows current preset")
	}
	if err := os.WriteFile(path, []byte(content+"\n用户自定义排版要求\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloadDialectsFromDirs(globalDir, workspaceDir)
	current = getDialect("standard")
	if current.Source != "global" || !strings.Contains(current.Prompt, "用户自定义排版要求") {
		t.Fatal("edited user preset was discarded")
	}
	composed, err := composeDialect("standard", nil, "测试")
	if err != nil || !strings.HasPrefix(composed.Prompt, commonCompilerRules) {
		t.Fatal("custom preset bypassed common compiler rules")
	}
}

func TestGeneratedMarkerTracksEditsAndFutureBuiltinUpdates(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	oldBody := "旧版自动导出的排版要求"
	content := "# Label: exported\n" + builtinPresetMarker + shortHash(oldBody) + " -->\n" + oldBody + "\n"
	path := filepath.Join(workspaceDir, "standard.md")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	reloadDialectsFromDirs(globalDir, workspaceDir)
	if got := getDialect("standard"); got.Source != "builtin" {
		t.Fatal("unmodified marked export blocked builtin updates")
	}
	if err := os.WriteFile(path, []byte(content+"用户添加段落\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloadDialectsFromDirs(globalDir, workspaceDir)
	if got := getDialect("standard"); got.Source != "workspace" || !strings.Contains(got.Prompt, "用户添加段落") {
		t.Fatal("modified marked export was not retained")
	}
}

func TestPresetReloadDoesNotSeedDiskAndExportsResolveToBuiltins(t *testing.T) {
	isolateCompilerLibraries(t)
	parent := t.TempDir()
	globalDir := filepath.Join(parent, "global")
	workspaceDir := filepath.Join(parent, "workspace")
	reloadDialectsFromDirs(globalDir, workspaceDir)
	if _, err := os.Stat(globalDir); !os.IsNotExist(err) {
		t.Fatal("reload unexpectedly created global preset files")
	}
	writeBuiltinDialects(globalDir, true)
	reloadDialectsFromDirs(globalDir, workspaceDir)
	for _, preset := range listSortedDialects() {
		if preset.Source != "builtin" {
			t.Fatalf("unedited export shadowed builtin: %s", preset.Name)
		}
	}
}

func TestBuiltinPresetsOnlyChoosePresentation(t *testing.T) {
	isolateCompilerLibraries(t)
	for name, preset := range builtinDialectTemplates {
		for _, forbidden := range []string{"100%", "45%", "55%", "42%", "最优帕累托", "userld→userId", "零空行", "全量通用算子表"} {
			if strings.Contains(preset.Prompt, forbidden) {
				t.Fatalf("%s contains obsolete claim/rule %q", name, forbidden)
			}
		}
		composed, err := composeDialect(name, nil, "禁止修改原文里的 userld 和 /orders 字面量")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(composed.Prompt, "不得自动纠正") || !strings.Contains(composed.Prompt, "必要条件不改成充分条件") {
			t.Fatalf("%s omitted common semantic/literal protections", name)
		}
	}
}
