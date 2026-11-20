package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These are deliberately narrow layout recognizers, not a Markdown parser or
// a density score. Uncertain text keeps its numbers and reaches semantic review.
var outlineLabelRE = regexp.MustCompile(`^([ ]{0,3})(?:#{1,6}[ \t]+)?([0-9]{1,3}|[一二三四五六七八九十百零〇]+)(\\?[.)、．])([ \t]*)(.+)$`)
var outlineMarkdownHeadingRE = regexp.MustCompile(`^#{1,6}[ \t]+`)
var outlineTreeNumberRE = regexp.MustCompile(`^(?:[0-9]{1,3}|[一二三四五六七八九十百零〇]+)[ \t]+(.+)$`)
var outlineFenceRE = regexp.MustCompile("(?m)^ {0,3}(`{3,}|~{3,})[^\\r\\n]*")
var outlineReferenceRE = regexp.MustCompile(`(?i)\b(?:step|section|chapter|item|rule|clause|paragraph|point)\s+(?:no\.?\s*)?[0-9]+|第\s*[0-9一二三四五六七八九十百零〇]+\s*(?:步|章|节|条|项|段|句|部分)|(?:步骤|章节|条款)\s*[0-9一二三四五六七八九十百零〇]+|(?:参见|详见|见)\s*[(（]?[0-9一二三四五六七八九十百零〇]+[.、)）]|\b(?:see|refer\s+to)\s+[(#]?[0-9]+(?:[.)]|$)`)
var outlinePreserveRE = regexp.MustCompile(`(?i)(?:原样保留|保留原|保持原|维持原|沿用原|必须保留|须保留|保留全部|保留所有).{0,12}(?:编号|序号|章节|提纲)|(?:不要|不得|禁止)(?:修改|改变|更改|删除|重排).{0,12}(?:编号|序号|章节)|(?:输出|回答|返回)(?:的)?(?:格式|模板|结构)\s*[:：]|(?:按|按照)(?:以下|下面|原有|指定).{0,12}(?:格式|模板).{0,8}(?:输出|回答|返回)|\b(?:preserve|retain|keep)\s+(?:the\s+)?(?:original\s+)?(?:numbering|numbers|section\s+numbers|outline)|\b(?:output|response|return)\s+(?:format|template|schema)\s*:`)
var outlineDateOrUnitRE = regexp.MustCompile(`(?i)^(?:january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|oct|nov|dec)(?:\b|\.)|^(?:元|万元|秒|分钟|小时|天|月|年|次|件|笔|位|毫秒|kg|g|ms|seconds?|minutes?|hours?|days?|mb|gb|tb|usd|cny)(?:$|\s)`)
var outlineActionRE = regexp.MustCompile(`(?i)^(?:if|when|unless|only|must|shall|please|check|verify|run|execute|send|wait|record|return|use|keep|preserve|ensure|allow|deny|open|save|read|write|delete|clear|retry)\b|^(?:如果|仅当|否则|必须|不得|允许|禁止|请|若|先|再|打开|保存|读取|写入|检查|校验|验证|记录|调用|发送|等待|修改|删除|重试|将)`)

type numberedOutlineLine struct {
	start, end int // Byte span of the label itself, excluding punctuation.
	body       string
	family     string
	number     int // Zero for Chinese labels; only Arabic labels feed numeric guards.
}

// Multi-section prose has a fixed tree-format contract. This checks visible
// organization only; semantic validation must still assess the relationships.
func babelStructureIssues(original, candidate string) []string {
	if outlineLabelsProtected(original) || outlineHasOnlyRelationBody(original) || len(outlineSourceTitles(original)) < 2 {
		return nil
	}
	var issues []string
	if titles := inheritedOutlineTitles(original, candidate); len(titles) > 0 {
		issues = append(issues, "候选仍复刻原章节标题: "+strings.Join(titles, "、"))
	}
	if treeBranchCount(candidate) < 2 {
		issues = append(issues, "多分支正文未组织为树：需至少两个带正文的树分支（├ / └ / ╰）")
	}
	return issues
}

func outlineSourceTitles(text string) map[string]bool {
	titles := make(map[string]bool)
	for _, line := range numberedOutlineLines(text) {
		if title := standaloneOutlineTitle(outlineHeadingLabel(line.body)); title != "" {
			titles[title] = true
		}
	}
	return titles
}

func treeBranchCount(text string) int {
	mask := outlineLiteralMask(text)
	count, offset := 0, 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimLeftFunc(raw, func(r rune) bool {
			return unicode.IsSpace(r) || r == '│' || r == '┃'
		})
		if line != "" {
			branch, width := utf8.DecodeRuneInString(line)
			start := offset + len(raw) - len(line)
			if (branch == '├' || branch == '└' || branch == '╰') && !mask[start] {
				body := strings.TrimLeftFunc(line[width:], func(r rune) bool {
					return unicode.IsSpace(r) || r >= '─' && r <= '╿'
				})
				if body != "" {
					count++
				}
			}
		}
		offset += len(raw) + 1
	}
	return count
}

// inheritedOutlineTitles detects reuse of the source's chapter organization.
// Removing numbering or decorating the same headings does not reorganize it.
func inheritedOutlineTitles(original, candidate string) []string {
	if outlineLabelsProtected(original) {
		return nil
	}
	originalTitles := outlineSourceTitles(original)
	if len(originalTitles) < 2 || outlineHasOnlyRelationBody(original) {
		return nil
	}
	mask := outlineLiteralMask(candidate)
	plain := []byte(candidate)
	for i, protected := range mask {
		if protected && plain[i] != '\n' && plain[i] != '\r' {
			plain[i] = ' '
		}
	}
	var copied []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(plain), "\n") {
		body := candidateOutlineHeading(line)
		title := standaloneOutlineTitle(body)
		if title != "" && originalTitles[title] && !seen[title] {
			copied = append(copied, body)
			seen[title] = true
		}
	}
	if len(copied) < 2 {
		return nil
	}
	return copied
}

// Match only a known source heading at the start of a candidate line. Tree
// strokes, Markdown styling and inline label:value bodies can disguise the
// same parallel chapters; their presence alone is not a restructuring signal.
func candidateOutlineHeading(raw string) string {
	line := strings.TrimSpace(raw)
	withoutTree := strings.TrimLeftFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || r >= '─' && r <= '╿'
	})
	hasTree := withoutTree != line
	if !hasTree && (strings.HasPrefix(raw, "    ") || strings.HasPrefix(raw, "\t")) {
		return "" // Preserve ordinary indented code.
	}
	line = withoutTree
	if len(line) >= 2 && strings.ContainsRune("-*+", rune(line[0])) && (line[1] == ' ' || line[1] == '\t') {
		line = strings.TrimSpace(line[2:])
	}
	line = outlineMarkdownHeadingRE.ReplaceAllString(line, "")
	if parts := outlineLabelRE.FindStringSubmatch(line); len(parts) != 0 {
		line = parts[5]
	} else if hasTree {
		if parts := outlineTreeNumberRE.FindStringSubmatch(line); len(parts) != 0 {
			line = parts[1]
		}
	}
	line = outlineHeadingLabel(line)
	if colon := strings.IndexAny(line, ":："); colon >= 0 {
		line = line[:colon]
	}
	return outlineHeadingLabel(line)
}

func outlineHeadingLabel(body string) string {
	for {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(body), ":"), "："))
		for _, marker := range []string{"**", "__"} {
			if len(trimmed) >= 2*len(marker) && strings.HasPrefix(trimmed, marker) && strings.HasSuffix(trimmed, marker) {
				trimmed = strings.TrimSpace(trimmed[len(marker) : len(trimmed)-len(marker)])
			}
		}
		if trimmed == body {
			return body
		}
		body = trimmed
	}
}

// An already compact source need not be restructured when every substantive
// body line expresses a relation or a simple label:value. This exemption is
// for the source, not decoration added to copied candidate chapters.
func outlineHasOnlyRelationBody(text string) bool {
	mask := outlineLiteralMask(text)
	plain := []byte(text)
	for i, protected := range mask {
		if protected {
			plain[i] = ' '
		}
	}
	seenRelation := false
	for _, raw := range strings.Split(string(plain), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if parts := outlineLabelRE.FindStringSubmatch(line); len(parts) != 0 {
			if standaloneOutlineTitle(strings.TrimSpace(parts[5])) != "" {
				continue
			}
			line = parts[5]
		}
		if strings.ContainsAny(line, "∧∨∀∃∈∉⊆→←⇒⇔=<>≤≥") {
			seenRelation = true
			continue
		}
		if colon := strings.IndexAny(line, ":："); colon > 0 && !strings.ContainsAny(line, "。！？!?，,") {
			seenRelation = true
			continue
		}
		return false
	}
	return seenRelation
}

// structuralNumberSpans ignores only labels in ordinary sequential lists which
// start at 1 and have at least a second item. Values inside item bodies remain
// untouched. References or explicit layout requirements conservatively disable
// this exemption for the whole text.
func structuralNumberSpans(text string) [][2]int {
	if outlineLabelsProtected(text) {
		return nil
	}
	groups := make(map[string][]numberedOutlineLine)
	for _, line := range numberedOutlineLines(text) {
		if line.number > 0 {
			groups[line.family] = append(groups[line.family], line)
		}
	}
	var spans [][2]int
	for _, lines := range groups {
		for i := 0; i < len(lines); {
			if lines[i].number != 1 {
				i++
				continue
			}
			end := i + 1
			for end < len(lines) && lines[end].number == end-i+1 {
				end++
			}
			if end-i >= 2 {
				for _, line := range lines[i:end] {
					spans = append(spans, [2]int{line.start, line.end})
				}
			}
			i = end
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	return spans
}

func numberedOutlineLines(text string) []numberedOutlineLine {
	protected := outlineLiteralMask(text)
	var result []numberedOutlineLine
	for offset := 0; offset < len(text); {
		end := strings.IndexByte(text[offset:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += offset
		}
		line := strings.TrimSuffix(text[offset:end], "\r")
		if loc := outlineLabelRE.FindStringSubmatchIndex(line); loc != nil {
			label := line[loc[4]:loc[5]]
			punctuation := strings.TrimPrefix(line[loc[6]:loc[7]], "\\")
			space := line[loc[8]:loc[9]]
			body := strings.TrimSpace(line[loc[10]:loc[11]])
			number, _ := strconv.Atoi(label)
			arabicLabel := label[0] >= '0' && label[0] <= '9'
			first, _ := utf8.DecodeRuneInString(body)
			start, stop := offset+loc[4], offset+loc[5]
			if body != "" && !unicode.IsDigit(first) && !outlineDateOrUnitRE.MatchString(body) &&
				!(arabicLabel && (punctuation == "." || punctuation == "．") && space == "") &&
				!outlineSpanProtected(protected, start, stop) {
				result = append(result, numberedOutlineLine{start, stop, body, line[loc[2]:loc[3]] + punctuation, number})
			}
		}
		offset = end + 1
	}
	return result
}

func standaloneOutlineTitle(body string) string {
	if strings.ContainsAny(body, "：:，,。.;；!?！？=<>≤≥≠∧∨∀∃∈∉→⇒↔⇔|+{}[]()（）`~\"'“”‘’「」『』") || outlineActionRE.MatchString(body) {
		return ""
	}
	for _, r := range body {
		if unicode.IsDigit(r) {
			return ""
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(body), " "))
}

func outlineLabelsProtected(text string) bool {
	mask := outlineLiteralMask(text)
	unquoted := []byte(text)
	for i, protected := range mask {
		if protected {
			unquoted[i] = ' '
		}
	}
	return outlineReferenceRE.Match(unquoted) || outlinePreserveRE.Match(unquoted)
}

func outlineSpanProtected(mask []bool, start, end int) bool {
	for _, protected := range mask[start:end] {
		if protected {
			return true
		}
	}
	return false
}

func outlineLiteralMask(text string) []bool {
	mask := make([]bool, len(text))
	mark := func(start, end int) {
		for i := start; i < end; i++ {
			mask[i] = true
		}
	}
	for _, code := range codeLiterals(text) {
		mark(code.start, code.end)
	}
	for _, fence := range outlineFenceRE.FindAllStringIndex(text, -1) {
		opener := fence[0]
		for opener < fence[1] && text[opener] == ' ' {
			opener++
		}
		if !mask[opener] {
			// An unmatched fence is still literal-looking text. Never remove
			// apparent list numbers from its remaining body.
			mark(fence[0], len(text))
			break
		}
	}
	for i := 0; i < len(text); {
		r, width := utf8.DecodeRuneInString(text[i:])
		if mask[i] {
			i += width
			continue
		}
		close := rune(0)
		switch r {
		case '"', '\'':
			if r == '\'' && i > 0 && isASCIIIdentifier(text[i-1]) {
				break // Apostrophe inside an English word.
			}
			close = r
		case '“':
			close = '”'
		case '‘':
			close = '’'
		case '「':
			close = '」'
		case '『':
			close = '』'
		}
		if close == 0 {
			i += width
			continue
		}
		end := len(text)
		for j := i + width; j < len(text); {
			next, nextWidth := utf8.DecodeRuneInString(text[j:])
			if next == '\\' && (close == '"' || close == '\'') && j+nextWidth < len(text) {
				_, escapedWidth := utf8.DecodeRuneInString(text[j+nextWidth:])
				j += nextWidth + escapedWidth
				continue
			}
			j += nextWidth
			if next == close {
				end = j
				break
			}
		}
		mark(i, end)
		i = end
	}
	return mask
}
