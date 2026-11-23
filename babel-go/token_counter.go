package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// TokenCount describes the count of ordinary text, without chat framing,
// tool definitions, image tokens, or other provider-specific request overhead.
// Exact means exact for Encoding, not verified usage of an inferred model.
type TokenCount struct {
	Tokens   int
	Kind     string
	Source   string
	Scope    string
	Encoding string
	Version  string
}

const tokenCounterVersion = "v0.3.0"

func normalizeTokenEncoding(encoding string) string {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	if encoding == "" {
		return "estimate"
	}
	return encoding
}

// supportedTokenEncodings returns a fresh slice so callers cannot mutate the
// process-wide set. Encoding selection is deliberately independent of models.
func supportedTokenEncodings() []string {
	return []string{"estimate", "cl100k_base", "o200k_base"}
}

func validateTokenEncoding(encoding string) error {
	switch normalizeTokenEncoding(encoding) {
	case "estimate", "cl100k_base", "o200k_base":
		return nil
	default:
		return fmt.Errorf("unsupported token encoding %q; choose estimate, cl100k_base, or o200k_base", encoding)
	}
}

type tokenCodecState struct {
	once  sync.Once
	codec tokenizer.Codec
	err   error
}

// Only Encode is used. Decode builds a mutable reverse vocabulary in this
// library version and must not be called on these shared codecs.
var tokenCodecs = map[string]*tokenCodecState{
	"cl100k_base": {},
	"o200k_base":  {},
}

// tokenCountCache 缓存确定性的精确计数：同一段文本在同一编码下重复分词是纯浪费。
// 键为文本 SHA256 + 编码；只缓存 exact 结果，估算与失败都不入缓存。
var tokenCountCache = newTTLLRU[string, TokenCount](4096)

const tokenCountCacheTTL = 24 * time.Hour

func measureTokens(text, encoding string) (TokenCount, error) {
	encoding = normalizeTokenEncoding(encoding)
	if err := validateTokenEncoding(encoding); err != nil {
		return TokenCount{}, err
	}
	if !utf8.ValidString(text) {
		return TokenCount{}, fmt.Errorf("cannot count tokens: text is not valid UTF-8")
	}
	if encoding == "estimate" {
		return TokenCount{
			Tokens: estimateTokensBPE(text), Kind: "estimated", Source: "heuristic-v2",
			Scope: "text", Encoding: "estimate", Version: "2",
		}, nil
	}

	cacheKey := ""
	if encoding != "estimate" {
		cacheKey = sha256Hex(text) + "\x00" + encoding
		if cached, ok := tokenCountCache.Get(cacheKey); ok {
			return cached, nil
		}
	}
	state := tokenCodecs[encoding]
	state.once.Do(func() {
		state.codec, state.err = tokenizer.Get(tokenizer.Encoding(encoding))
	})
	if state.err != nil {
		return TokenCount{}, fmt.Errorf("cannot initialize token encoding %s: %w", encoding, state.err)
	}
	// This port's Encode treats special-token-looking strings as ordinary text.
	// User content such as <|endoftext|> is never promoted to a control token.
	ids, _, err := state.codec.Encode(text)
	if err != nil {
		return TokenCount{}, fmt.Errorf("cannot count tokens with %s: %w", encoding, err)
	}
	counted := TokenCount{
		Tokens: len(ids), Kind: "exact", Source: "tiktoken-go/tokenizer",
		Scope: "text", Encoding: encoding, Version: tokenCounterVersion,
	}
	if cacheKey != "" {
		tokenCountCache.Put(cacheKey, counted, tokenCountCacheTTL)
	}
	return counted, nil
}
