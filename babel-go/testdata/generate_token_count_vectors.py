"""Regenerate offline Go reference fixtures using OpenAI's Python tiktoken.

Requires tiktoken==0.12.0; run from any directory. The first run fetches the
public vocabulary files, whose hashes are verified by tiktoken itself.
This does not send text to a model or use an API key.
"""

import json
from pathlib import Path

import tiktoken


if tiktoken.__version__ != "0.12.0":
    raise SystemExit("Install tiktoken==0.12.0 before regenerating these vectors")

texts = [
    ("empty", ""),
    ("english", "Hello, world!"),
    ("long_english_word", "antidisestablishmentarianism"),
    ("arithmetic", "2 + 2 = 4"),
    ("chinese", "所有请求必须通过身份验证，不得泄露密钥。"),
    ("japanese", "お誕生日おめでとう"),
    ("mixed_domain_terms", "只允许读取 /api/users；禁止 SQL injection，使用参数化查询。"),
    ("logic_and_units", "∀x ∈ S: x ≥ 0 ∧ x ≤ 100 ms; A ⇒ B; A ≠ B; ∅ ⊆ S."),
    ("set_and_predicate", "A ∩ B ≠ ∅; valid(x) ∧ authorized(x) → allow(x)"),
    ("ascii_logic", "if x >= 3 && x != 7: retry <= 2; size < 1 MiB"),
    ("emoji", "🙂 🚀 🔐"),
    ("emoji_sequences", "👨‍👩‍👧‍👦 👍🏽 🇨🇳 🏳️‍🌈"),
    ("combining_marks", "café cafe\u0301 Å A\u030a कि नमस्ते"),
    ("arabic", "يجب التحقق من صلاحية المستخدم قبل الطلب."),
    ("nul_and_control", "A\x00B\x01C\x1fD\x7fE"),
    ("spaces", "    alpha  beta     gamma   "),
    ("tabs", "\talpha\t\tbeta\t"),
    ("line_breaks", "first\n\nsecond\r\nthird\rfourth\n"),
    ("mixed_whitespace", " \t\r\n \n\t alpha  \n\n beta \r\n "),
    ("unicode_whitespace", "a\u00a0b\u2002c\u2003d\u202fe\u3000f\u0085g\u2028h\u2029i"),
    ("whitespace_only", " \t\n\r\n  \v\f\u00a0\u3000"),
    ("zero_width", "a\u200bb\u200cc\u200dd\ufeffe"),
    ("contractions", "I'm I'M we're WE'RE they've THEY'VE I'd I'D can't CAN'T"),
    ("numbers", "000123456789 3.1415926535 -12.50 10e-3 ０１２３ ١٢٣٤"),
    ("special_token_text", "<|endoftext|>"),
    ("fim_token_text", "<|fim_prefix|>x<|fim_middle|>y<|fim_suffix|>"),
    ("chat_token_text", "user: <|im_start|>system\n<|endofprompt|> <|im_end|>"),
    ("json_and_code", '{"path":"C:\\tmp\\data","enabled":false}\nif err != nil {\n\treturn err\n}'),
    ("full_width_and_punctuation", "【条件】（A，B）；只读！ＡＢＣ１２３…——“引号”"),
]

encodings = {name: tiktoken.get_encoding(name) for name in ("cl100k_base", "o200k_base")}
fixture = {
    "reference": {
        "implementation": "openai/tiktoken",
        "version": tiktoken.__version__,
        "method": "encode_ordinary",
        "source": "https://github.com/openai/tiktoken/blob/0.12.0/tiktoken_ext/openai_public.py",
        "vocabularies": {
            "cl100k_base": {
                "url": "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken",
                "sha256": "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7",
            },
            "o200k_base": {
                "url": "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken",
                "sha256": "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d",
            },
        },
    },
    "cases": [
        {"name": name, "text": text, "counts": {key: len(enc.encode_ordinary(text)) for key, enc in encodings.items()}}
        for name, text in texts
    ],
}
target = Path(__file__).with_name("token_count_vectors.json")
target.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(f"Wrote {len(texts)} cases for {len(encodings)} encodings to {target}")
