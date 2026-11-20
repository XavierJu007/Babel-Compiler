package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

var quotedLiteralRegex = regexp.MustCompile(`"(?:\\[^\r\n]|[^"\\\r\n])*"|'(?:\\[^\r\n]|[^'\\\r\n])*'|“[^”\r\n]*”|‘[^’\r\n]*’|「[^」\r\n]*」|『[^』\r\n]*』`)
var exactQuoteContext = regexp.MustCompile(`(?i)逐字|原样|不得改写|不可改写|不翻译|保留为|\b(?:verbatim|literal|exact(?:ly)?|do not translate|don't translate)\b`)
var optionalExactQuoteContext = regexp.MustCompile(`(?:无需|无须|不必|不用|不需要|不要求)\s*(?:(?:保持|保留)\s*)?(?:逐字|原样)(?:\s*保留(?:为)?)?`)
var exactQuoteSuffix = regexp.MustCompile(`(?i)^\s*(?:(?:必须|需要|需|须|应|要|请)\s*)?(?:(?:保持|保留)\s*)?(?:逐字|原样|不得改写|不可改写|不翻译)|^\s*(?:must\s+be\s+)?(?:kept\s+)?(?:verbatim|exact(?:ly)?)\b`)
var fixedQuoteContext = regexp.MustCompile(`(?i)(?:返回|回复|输出)(?:固定(?:字符串|文本|值)?|字符串|字面量|文本|值)?\s*(?:为|是|:|：)?\s*$|(?:枚举(?:值)?|状态码|错误码|字符串)\s*(?:为|是|包括|取值为|取|含|:|：)?\s*$|\b(?:return|reply|output|header|payload|label|status|enum|string)\s*(?:value\s*)?(?:is|as|to|=|:)?\s*$`)
var quoteListSeparator = regexp.MustCompile(`(?i)^\s*(?:[,，、/|]|或|和|及|与|and|or)\s*$`)
var fieldValueSuffix = regexp.MustCompile(`^\s*(?:写入|存入|赋值给)\s*[A-Za-z_]`)

type quotedText struct {
	start, end int
	raw, value string
}

func quotedTexts(s string) []quotedText {
	var result []quotedText
	for offset := 0; offset < len(s); {
		loc := quotedLiteralRegex.FindStringIndex(s[offset:])
		if loc == nil {
			break
		}
		start, end := offset+loc[0], offset+loc[1]
		raw := s[start:end]
		offset = end
		if raw[0] == '\'' && (start > 0 && isASCIIIdentifier(s[start-1]) || end < len(s) && isASCIIIdentifier(s[end])) {
			offset = start + 1 // An English apostrophe may precede a real quote.
			continue
		}
		_, left := utf8.DecodeRuneInString(raw)
		_, right := utf8.DecodeLastRuneInString(raw)
		result = append(result, quotedText{start, end, raw, raw[left : len(raw)-right]})
	}
	return result
}

// Decode actual JSON containers, including strings after null, numbers or
// nested objects. Mere surrounding prose is not a machine string declaration.
func jsonContainerSpans(s string) [][2]int {
	var spans [][2]int
	for offset := 0; offset < len(s); {
		at := strings.IndexAny(s[offset:], "[{")
		if at < 0 {
			break
		}
		start := offset + at
		decoder := json.NewDecoder(strings.NewReader(s[start:]))
		var value json.RawMessage
		if err := decoder.Decode(&value); err == nil {
			offset = start + int(decoder.InputOffset())
			spans = append(spans, [2]int{start, offset})
		} else {
			offset = start + 1
		}
	}
	return spans
}

// Quotation marks alone do not make prose a byte-level contract. These narrow
// guards recognize explicit exact wording, fixed outputs and string syntax;
// ordinary descriptions still undergo the same semantic comparison as prose.
func protectedQuotedLiterals(s string) []string {
	quotes := quotedTexts(s)
	containers := jsonContainerSpans(s)
	containerIndex := 0
	var result []string
	previousProtected, previousSyntax := false, false
	for i, quote := range quotes {
		left, right := 0, len(s)
		if i > 0 {
			left = quotes[i-1].end
		}
		if i+1 < len(quotes) {
			right = quotes[i+1].start
		}
		prefix, suffix := s[left:quote.start], s[quote.end:right]
		// Evidence is local to this clause and cannot leak across another quote.
		if at := strings.LastIndexAny(prefix, "\n\r。！？!?；;，,."); at >= 0 {
			_, size := utf8.DecodeRuneInString(prefix[at:])
			prefix = prefix[at+size:]
		}
		if at := strings.IndexAny(suffix, "\n\r。！？!?；;，,."); at >= 0 {
			suffix = suffix[:at]
		}
		before := strings.TrimSpace(s[:quote.start])
		between := ""
		sameLine := false
		if i > 0 {
			separator := s[quotes[i-1].end:quote.start]
			between = strings.TrimSpace(separator)
			sameLine = !strings.ContainsAny(separator, "\r\n")
		}
		asciiQuote := quote.raw[0] == '"' || quote.raw[0] == '\''
		for containerIndex < len(containers) && quote.start >= containers[containerIndex][1] {
			containerIndex++
		}
		inJSON := containerIndex < len(containers) && quote.start >= containers[containerIndex][0] && quote.end <= containers[containerIndex][1]
		// A prose heading followed by ':' is not JSON. Actual containers above
		// establish JSON string syntax, including multiline objects and arrays.
		syntax := inJSON || asciiQuote && (strings.HasSuffix(before, "=") || strings.HasSuffix(before, "[") ||
			previousSyntax && sameLine && between == ",")
		// An explicit waiver such as 无需逐字保留 is not a byte contract. Remove
		// only that local phrase so a separate positive requirement still applies.
		exactPrefix := optionalExactQuoteContext.ReplaceAllString(prefix, "")
		exact := syntax || quote.value == "" || strings.Contains(quote.value, "\\") ||
			exactQuoteContext.MatchString(exactPrefix) || exactQuoteSuffix.MatchString(suffix)
		fixed := fixedQuoteContext.MatchString(prefix) || fieldValueSuffix.MatchString(suffix) ||
			previousProtected && sameLine && (between == "" || quoteListSeparator.MatchString(between))
		if exact {
			result = append(result, quote.raw)
		} else if fixed {
			// Decorative delimiters may change; the returned/defined value may not.
			result = append(result, quote.value)
		}
		previousProtected, previousSyntax = exact || fixed, syntax
	}
	return result
}
