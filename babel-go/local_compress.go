package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// 短文本经过 LLM 重写成巴别语时，结构标签反而容易造成 token 膨胀且白等数秒；
	// 仅当文本达到最小长度和最小 token 收益门槛时才值得交给 LLM 编译。
	autoMinLLMRunes  = 180
	autoMinLLMTokens = 80
)

var (
	localHRRuleRE      = regexp.MustCompile(`^(?:-[ \t]*){3,}$|^(?:=[ \t]*){3,}$|^(?:\*[ \t]*){3,}$|^(?:_[ \t]*){3,}$`)
	localTableSepCellRE = regexp.MustCompile(`^(:?)-{3,}(:?)$`)
	localListPrefixRE  = regexp.MustCompile(`^(?:[-*+]\s+|\d+[.)]\s+)`)
	localURLRE         = regexp.MustCompile(`https?://[^\s<>()]+`)
)

type localBoilerplateRule struct {
	prefix      string
	replacement string
	lineStart   bool
	caseFold    bool
}

var localBoilerplateRules = []localBoilerplateRule{
	{prefix: "It is important to note that ", replacement: "", lineStart: true, caseFold: true},
	{prefix: "Please be advised that ", replacement: "", lineStart: true, caseFold: true},
	{prefix: "Please note that ", replacement: "", lineStart: true, caseFold: true},
	{prefix: "Kindly note that ", replacement: "", lineStart: true, caseFold: true},
	{prefix: "Keep in mind that ", replacement: "", lineStart: true, caseFold: true},
	{prefix: "In order to ", replacement: "To ", lineStart: true, caseFold: false},
	{prefix: " in order to ", replacement: " to ", lineStart: false, caseFold: false},
	{prefix: "需要注意的是，", replacement: "", lineStart: true, caseFold: false},
	{prefix: "需要注意的是,", replacement: "", lineStart: true, caseFold: false},
	{prefix: "请注意，", replacement: "", lineStart: true, caseFold: false},
	{prefix: "请注意,", replacement: "", lineStart: true, caseFold: false},
	{prefix: "温馨提示：", replacement: "", lineStart: true, caseFold: false},
	{prefix: "特别说明：", replacement: "", lineStart: true, caseFold: false},
}

func buildLocalProtectionMask(text string) []bool {
	mask := outlineLiteralMask(text)
	for _, span := range localURLRE.FindAllStringIndex(text, -1) {
		for i := span[0]; i < span[1] && i < len(mask); i++ {
			mask[i] = true
		}
	}
	return mask
}

func isUnmaskedRange(mask []bool, start, end int) bool {
	if start < 0 || end > len(mask) {
		return false
	}
	for i := start; i < end; i++ {
		if mask[i] {
			return false
		}
	}
	return true
}

func compactMarkdownTableLine(line string, lineMask []bool) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") || strings.Count(trimmed, "|") < 2 {
		return line
	}
	// Preserve leading indentation exactly; compact only cell padding adjacent to unmasked pipes.
	lead := 0
	for lead < len(line) && (line[lead] == ' ' || line[lead] == '\t') {
		lead++
	}
	indent := line[:lead]
	body := line[lead:]
	bodyMask := lineMask[lead:]
	trailEnd := len(body)
	for trailEnd > 0 && (body[trailEnd-1] == ' ' || body[trailEnd-1] == '\t') && !bodyMask[trailEnd-1] {
		trailEnd--
	}
	body = body[:trailEnd]
	bodyMask = bodyMask[:trailEnd]

	type cellSlice struct {
		text     string
		unmasked bool
	}
	var cells []cellSlice
	cellStart := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '|' && !bodyMask[i] && (i == 0 || body[i-1] != '\\') {
			seg := body[cellStart:i]
			segMask := bodyMask[cellStart:i]
			l := 0
			for l < len(seg) && (seg[l] == ' ' || seg[l] == '\t') && !segMask[l] {
				l++
			}
			r := len(seg)
			for r > l && (seg[r-1] == ' ' || seg[r-1] == '\t') && !segMask[r-1] {
				r--
			}
			cells = append(cells, cellSlice{
				text:     seg[l:r],
				unmasked: isUnmaskedRange(segMask, l, r),
			})
			cellStart = i + 1
		}
	}
	if len(cells) < 2 {
		return line
	}
	// Trailing text after the closing pipe should be empty because trimmed ended with '|'.
	tail := body[cellStart:]
	if strings.TrimSpace(tail) != "" {
		return line
	}
	inner := cells[1:] // cells[0] before leading '|' is empty.
	allSep := len(inner) > 0
	for _, c := range inner {
		if !c.unmasked || !localTableSepCellRE.MatchString(c.text) {
			allSep = false
			break
		}
	}
	var out strings.Builder
	out.Grow(len(line))
	out.WriteString(indent)
	out.WriteByte('|')
	for _, c := range inner {
		cellText := c.text
		if allSep {
			cellText = localTableSepCellRE.ReplaceAllString(cellText, "${1}---${2}")
		}
		out.WriteString(cellText)
		out.WriteByte('|')
	}
	return out.String()
}

func stripUnmaskedBoilerplate(line string, lineMask []bool) string {
	if len(lineMask) != len(line) {
		return line
	}
	lead := 0
	for lead < len(line) && (line[lead] == ' ' || line[lead] == '\t') {
		lead++
	}
	prefixEnd := lead
	if loc := localListPrefixRE.FindStringIndex(line[lead:]); loc != nil && loc[0] == 0 {
		prefixEnd = lead + loc[1]
	}

	// Only rewrite exact leading phrase at content start if that span is unmasked.
	for _, rule := range localBoilerplateRules {
		if !rule.lineStart {
			continue
		}
		candStart := prefixEnd
		candEnd := candStart + len(rule.prefix)
		if candEnd >= len(line) || !isUnmaskedRange(lineMask, candStart, candEnd) {
			continue
		}
		matched := false
		if rule.caseFold {
			matched = strings.EqualFold(line[candStart:candEnd], rule.prefix)
		} else {
			matched = line[candStart:candEnd] == rule.prefix
		}
		if !matched {
			continue
		}
		rest := line[candEnd:]
		if strings.TrimSpace(rest) == "" {
			continue
		}
		// Avoid altering capitalization awkwardly when dropping an uppercase English lead-in
		// would leave a lowercase letter unless a replacement capitalizes it.
		line = line[:candStart] + rule.replacement + rest
		return line
	}

	// Safe mid-sentence literal replacement (" in order to " -> " to ") only when unmasked.
	for _, rule := range localBoilerplateRules {
		if rule.lineStart {
			continue
		}
		idx := strings.Index(line[prefixEnd:], rule.prefix)
		for idx >= 0 {
			absStart := prefixEnd + idx
			absEnd := absStart + len(rule.prefix)
			if absEnd < len(line) && isUnmaskedRange(lineMask, absStart, absEnd) {
				updated := line[:absStart] + rule.replacement + line[absEnd:]
				// Mask lengths shift after rewrite; one safe match per pass avoids mask drift.
				return updated
			}
			nextOffset := absEnd
			if nextOffset >= len(line) {
				break
			}
			rel := strings.Index(line[nextOffset:], rule.prefix)
			if rel < 0 {
				break
			}
			idx = (nextOffset - prefixEnd) + rel
		}
	}
	return line
}

func collapseUnmaskedInlineSpaces(line string, lineMask []bool) string {
	if len(lineMask) != len(line) {
		return line
	}
	lead := 0
	for lead < len(line) && (line[lead] == ' ' || line[lead] == '\t') {
		lead++
	}
	var b strings.Builder
	b.Grow(len(line))
	b.WriteString(line[:lead])
	pendingSpace := false
	for i := lead; i < len(line); {
		r, w := utf8.DecodeRuneInString(line[i:])
		unmasked := isUnmaskedRange(lineMask, i, i+w)
		if unmasked && (r == ' ' || r == '\t') {
			pendingSpace = true
			i += w
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteString(line[i : i+w])
		i += w
	}
	if pendingSpace {
		// Trailing unmasked spaces are stripped.
	}
	return b.String()
}

// localCompressDeterministic 在毫秒内执行零 LLM、严格保真（避开代码围栏/引文/URL/字面量/数字/否定）
// 的本地确定性压缩；仅当本地不变量全部通过且目标 Token 严格减少时才会采用。
func localCompressDeterministic(original, encoding string, requiredKeywords []string) (compressed string, origTok, compTok int, changed bool) {
	origCount, err := measureTokens(original, encoding)
	if err != nil {
		return original, 0, 0, false
	}
	origTok = origCount.Tokens

	trimmed := strings.TrimSpace(original)
	if trimmed == "" {
		return original, origTok, origTok, false
	}

	// 1) Pure JSON fast-path: minify whitespace cleanly when valid.
	first, last := trimmed[0], trimmed[len(trimmed)-1]
	if ((first == '{' && last == '}') || (first == '[' && last == ']')) && json.Valid([]byte(trimmed)) {
		var buf bytes.Buffer
		if json.Compact(&buf, []byte(trimmed)) == nil {
			cand := buf.String()
			if cand != "" && cand != original && len(checkDeterministicInvariants(original, cand, requiredKeywords)) == 0 {
				if after, mErr := measureTokens(cand, encoding); mErr == nil && after.Tokens < origTok {
					return cand, origTok, after.Tokens, true
				}
			}
		}
		return original, origTok, origTok, false
	}

	// 2) Prose / Markdown lossless pass with full literal mask protection.
	mask := buildLocalProtectionMask(original)
	type rawLine struct {
		start int
		end   int
		text  string
		mask  []bool
	}
	var lines []rawLine
	pos := 0
	for pos <= len(original) {
		relNL := strings.IndexByte(original[pos:], '\n')
		end := len(original)
		if relNL >= 0 {
			end = pos + relNL
		}
		contentEnd := end
		if contentEnd > pos && original[contentEnd-1] == '\r' && !mask[contentEnd-1] {
			contentEnd--
		}
		lines = append(lines, rawLine{
			start: pos,
			end:   contentEnd,
			text:  original[pos:contentEnd],
			mask:  mask[pos:contentEnd],
		})
		if relNL < 0 {
			break
		}
		pos = end + 1
	}

	outLines := make([]string, 0, len(lines))
	blankRun := 0
	inFrontmatter := false
	prevRawBlank := true
	for idx, ln := range lines {
		lineText := ln.text
		lineMask := ln.mask

		// Strip trailing horizontal whitespace while unmasked.
		tEnd := len(lineText)
		for tEnd > 0 && (lineText[tEnd-1] == ' ' || lineText[tEnd-1] == '\t') && !lineMask[tEnd-1] {
			tEnd--
		}
		lineText = lineText[:tEnd]
		lineMask = lineMask[:tEnd]

		allUnmasked := isUnmaskedRange(lineMask, 0, len(lineMask))
		trimmedLine := strings.TrimSpace(lineText)

		// Preserve YAML frontmatter delimiters and contents intact.
		if idx == 0 && allUnmasked && trimmedLine == "---" {
			inFrontmatter = true
			prevRawBlank = false
			outLines = append(outLines, lineText)
			continue
		}
		if inFrontmatter {
			if allUnmasked && (trimmedLine == "---" || trimmedLine == "...") {
				inFrontmatter = false
			}
			prevRawBlank = trimmedLine == ""
			outLines = append(outLines, lineText)
			continue
		}

		// Drop standalone decorative horizontal rules when preceded by a blank line
		// (preserving Setext H1/H2 underlines like "Heading\n---").
		if prevRawBlank && allUnmasked && trimmedLine != "" && localHRRuleRE.MatchString(trimmedLine) {
			prevRawBlank = true
			continue
		}
		prevRawBlank = trimmedLine == ""

		// Collapse excess unmasked blank lines (keep at most 1 blank line between blocks).
		if trimmedLine == "" && allUnmasked {
			if len(outLines) == 0 {
				continue
			}
			blankRun++
			if blankRun > 1 {
				continue
			}
			outLines = append(outLines, "")
			continue
		}
		blankRun = 0

		// Table formatting vs ordinary inline spacing & boilerplate cleanup.
		if strings.HasPrefix(trimmedLine, "|") && strings.HasSuffix(trimmedLine, "|") && strings.Count(trimmedLine, "|") >= 2 {
			lineText = compactMarkdownTableLine(lineText, lineMask)
		} else {
			collapsed := collapseUnmaskedInlineSpaces(lineText, lineMask)
			if len(collapsed) == len(lineText) {
				// Mask offsets remain valid when no bytes were removed.
				lineText = stripUnmaskedBoilerplate(collapsed, lineMask)
			} else {
				// Recompute slice-local mask after collapsing spaces so boilerplate check stays aligned.
				subMask := buildLocalProtectionMask(collapsed)
				lineText = stripUnmaskedBoilerplate(collapsed, subMask)
			}
		}
		outLines = append(outLines, lineText)
	}

	for len(outLines) > 0 && outLines[len(outLines)-1] == "" {
		outLines = outLines[:len(outLines)-1]
	}

	candidate := strings.Join(outLines, "\n")
	if strings.TrimSpace(candidate) == "" || candidate == original {
		return original, origTok, origTok, false
	}
	if len(checkDeterministicInvariants(original, candidate, requiredKeywords)) > 0 {
		return original, origTok, origTok, false
	}
	afterCount, err := measureTokens(candidate, encoding)
	if err != nil || afterCount.Tokens >= origTok {
		return original, origTok, origTok, false
	}
	return candidate, origTok, afterCount.Tokens, true
}

func normalizeCompileStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "local", "deep":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

// buildLocalFastCompileResp 构造短文本直通或本地无损压缩的完整 CompileResp。
func buildLocalFastCompileResp(original, compiled, dialectName, encoding, strategy, reason string, cacheHit bool) (CompileResp, error) {
	out := CompileResp{
		Compiled:   compiled,
		Dialect:    dialectName,
		CacheHit:   cacheHit,
		Strategy:   strategy,
		Reason:     reason,
		Validation: newValidation("passed", "local_fast"),
	}
	if err := fillCompileStats(&out, original, encoding); err != nil {
		return out, err
	}
	out.Audit = fmt.Sprintf("[PASS] %s；无需模型往返", reason)
	return out, nil
}

// tryFastCompileStrategy 根据 CompileReq.Strategy 决定是否走 <1ms 本地压缩 / 短文本直通：
//   - Strategy == "" 或 "deep"：返回 handled=false，保持现有 LLM 编译与单测/eval 行为完全一致。
//   - Strategy == "local"：永远只做 Tier-0 本地压缩，零 LLM 调用。
//   - Strategy == "auto"：非自然语言载荷、短于 180 runes 或短于 80 tokens 时走 Tier-0 / 直通，跳过 LLM 避免等待与膨胀。
func tryFastCompileStrategy(req CompileReq, p Profile, dialect Dialect) (CompileResp, bool, error) {
	strategy := normalizeCompileStrategy(req.Strategy)
	if strategy != "local" && strategy != "auto" {
		return CompileResp{}, false, nil
	}

	trimmed := strings.TrimSpace(req.Text)
	origCount, err := measureTokens(req.Text, p.TokenEncoding)
	if err != nil {
		return CompileResp{}, true, &TokenMeasurementError{Encoding: p.TokenEncoding, Err: err}
	}

	if strategy == "local" {
		comp, _, _, changed := localCompressDeterministic(req.Text, p.TokenEncoding, req.RequiredKeywords)
		if changed {
			resp, buildErr := buildLocalFastCompileResp(
				req.Text, comp, dialect.Name, p.TokenEncoding,
				"local_fast", "极速本地无损压缩（<1ms，零 LLM 调用）", false,
			)
			return resp, true, buildErr
		}
		resp, buildErr := buildLocalFastCompileResp(
			req.Text, req.Text, dialect.Name, p.TokenEncoding,
			"passthrough", "原文已足够紧凑，原样直通（<1ms）", false,
		)
		return resp, true, buildErr
	}

	// strategy == "auto"
	runeLen := utf8.RuneCountInString(trimmed)
	if isNonProsePayload(req.Text) || runeLen < autoMinLLMRunes || origCount.Tokens < autoMinLLMTokens {
		comp, _, _, changed := localCompressDeterministic(req.Text, p.TokenEncoding, req.RequiredKeywords)
		if changed {
			resp, buildErr := buildLocalFastCompileResp(
				req.Text, comp, dialect.Name, p.TokenEncoding,
				"local_fast", "短文本/结构化内容采用本地无损压缩（<1ms），跳过 LLM 避免耗时与膨胀", false,
			)
			return resp, true, buildErr
		}
		resp, buildErr := buildLocalFastCompileResp(
			req.Text, req.Text, dialect.Name, p.TokenEncoding,
			"passthrough", fmt.Sprintf("短文本低于 LLM 压缩收益阈值（<%d 字或 <%d token），原样直通（<1ms）", autoMinLLMRunes, autoMinLLMTokens), false,
		)
		return resp, true, buildErr
	}
	return CompileResp{}, false, nil
}
