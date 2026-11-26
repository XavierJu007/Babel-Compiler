# Babel-Compiler

[English](README.md) | [简体中文](README.zh-CN.md)

![Babel anime girl poster](babel-go/assets/babel-poster.png)

**A small tool that compiles natural-language prompts into compact Babel notation.**

- **Compact**: One fixed mixed-language shallow-tree format uses public terminology, abbreviations and symbols to express actors, conditions and dependencies.
- **Self-explanatory**: No private codebook or extra legend; write your own domain dictionaries.
- **Verifiable**: Text token counts and semantic equivalence checks; the proxy adopts a candidate only if the current check passes and its token count is lower, otherwise it keeps the original.

## Example

> For each payment callback, verify the signature, then check that it covers order_id and amount. Allow the order to change from pending to paid only when both checks pass and its current state is pending; otherwise, do not modify the order.

```text
∀payment callback
├flow: verify signature→check{order_id, amount}⊆signed fields
├only if both checks pass∧order.state=pending: may pending→paid
└else: do not modify order
```

Illustrative, not a measured model output. Savings depend on the input, model and encoding; passing a check does not guarantee lossless compression. [Full refund SOP example (Chinese)](docs/examples/README.md).

## Quick start

Download the package for your system from **[Releases](https://github.com/XavierJu007/Babel-Compiler/releases/latest)**. Choose an asset with the suffix below, rather than GitHub's **Source code** downloads. The program includes the UI and artwork; no Go installation or extra files from this repository are needed.

| System | Package suffix |
| --- | --- |
| Windows | Intel/AMD: `windows_amd64.zip` · ARM: `windows_arm64.zip` |
| macOS | Apple Silicon: `macos_arm64.tar.gz` · Intel: `macos_amd64.tar.gz` |
| Linux | x86-64: `linux_amd64.tar.gz` · ARM64: `linux_arm64.tar.gz` |

1. Extract the package. **Windows:** double-click `babel.exe` and keep its console window open. **macOS/Linux:** open a terminal in the extracted folder and run `./babel`.
2. Babel selects an available port in `127.0.0.1:8080–8099` and tries to open your browser. If it does not, open the URL printed in the terminal.
3. Supply your own OpenAI Chat Completions-compatible model service: save its request URL, API key and model name in the model settings, then paste a prompt to compile. Create your own dictionaries in the domain library.

**Complete endpoints receive POST requests unchanged.** Host-only URLs and base URLs ending in `/v1` get `/chat/completions` appended for compatibility. Remote services require HTTPS; HTTP is allowed only on loopback. [Endpoint and validation guide (Chinese)](docs/usage/validation.md).

<details>
<summary>Build from source (developers, Go 1.22+)</summary>

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

</details>

## CLI and API

Run these commands in the extracted folder; on Windows, use `.\babel.exe` in place of `./babel`. Check your version with `./babel -version`.

```bash
./babel -gate "Your prompt"
./babel -gate -q -f prompt.md > compiled.txt
```

Start a fixed port with `./babel -serve 127.0.0.1:8080`, then call the API from another terminal:

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"Your prompt","strict_gate":true}'
```

## Transparent proxy

Set the final service's base URL as `upstream_url` in the model settings, then point your client at the corresponding local endpoint:

| Protocol | Endpoint | Upstream authentication supplied by the caller | Behavior |
| --- | --- | --- | --- |
| OpenAI Chat | `/v1/chat/completions` | `Authorization` or `api-key` | Compresses `system`, `user` and tool descriptions |
| OpenAI Responses | `/v1/responses` | `Authorization` or `api-key` | Compresses `instructions`, `input` and tool descriptions |
| Anthropic | `/v1/messages` | `x-api-key` or `Authorization` | Compresses `system`, `user` and tool descriptions |
| Other endpoints | `/v1/*` (e.g. `/v1/models`, `/v1/messages/count_tokens`, `/v1/embeddings`) | Same as above | Zero-parse streaming passthrough |

**Compilation uses the saved profile; final execution uses `model` from the request body.** The caller must provide the upstream key; the proxy never borrows the compiler key.

The proxy replaces only eligible text fields while preserving roles, tool schemas, query strings and client headers; model responses (including long SSE streams) pass through directly without re-translation. Supports `X-Babel-Dry-Run: true` (zero-risk ROI observation mode), `X-Babel-Bypass: 1`, `X-Babel-Compile-Budget`, `X-Babel-Skip-Cache-Control: true`, live Web UI policy toggles, per-log redacted diagnostic bundles, and `GET /healthz`. [Full setup and fallback rules (Chinese)](docs/usage/validation.md#4-代理使用).

### Why It Saves Money (Break-Even Economics)

Calling an LLM to compile prompts only pays off when the **compiler is far cheaper than the consumer** and **compressed prefixes are reused**:

1. **Cheap or free compiler -> Frontier consumer**: Point Babel's compiler/auditor profile at a local Ollama model (e.g. `qwen2.5:7b` at \$0 marginal cost) or a low-cost model (`gpt-4o-mini` / `deepseek-chat`), while your downstream client calls Claude Sonnet/Opus or GPT-5.
2. **Amortized over repeated agent turns**: In multi-turn coding agents and MCP workflows, long `system` prompts and `tools` schema descriptions stay identical across 10–100+ turns. Babel compiles once in the background (`async_warmup`), then hits its dual LRU generation + validation cache with ~0 ms proxy overhead on every subsequent turn.

### Try Safely with Dry-Run Mode

Not ready to rewrite live production prompts? Enable **Dry-Run 观察模式** in the Web UI proxy policy bar (or start with `BABEL_PROXY_DRY_RUN=true` / send `X-Babel-Dry-Run: true`):

- Every request forwards **100% untouched** to upstream (zero semantic risk, zero rewrite dependency).
- Babel measures in the background how many tokens and what percentage would be saved across your real workflow and surfaces a live session ROI summary in the Web UI.

### 60-Second Client Setup Recipes

1. **Claude Code (Anthropic)**: Configure `upstream_url` (e.g. `https://api.anthropic.com`) in Model Settings, then run:
   ```bash
   export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
   claude
   ```
2. **Codex CLI / OpenAI SDK**:
   ```bash
   export OPENAI_BASE_URL="http://127.0.0.1:8080/v1"
   codex
   ```
3. **Zero-Cost Local Compiler (Ollama)**: In Model Settings set BaseURL `http://127.0.0.1:11434/v1/chat/completions`, API Key `ollama`, Model `qwen2.5:7b` (and keep `upstream_url` pointed at your cloud provider).
4. **Cursor / Cline / Cherry Studio**: Set your OpenAI/Anthropic base URL to `http://127.0.0.1:8080/v1` (or `http://127.0.0.1:8080`); if a desktop/web client sends a browser `Origin` header, start Babel with `BABEL_PROXY_ALLOWED_ORIGINS="http://localhost:3000"`.

### Docker Sidecar

Babel intentionally binds strictly to loopback (`127.0.0.1:8080`) so credentials and prompt traffic stay local:

```bash
docker compose up -d --build
```

See [docker-compose.yml](docker-compose.yml) for `BABEL_PROXY_DRY_RUN=true` and container sidecar (`network_mode: "service:babel"`) examples.

### Real-Model Evaluation (`babel-eval`)

Once your Babel server is running with a saved profile, measure real compression and validation pass rates on the bundled cases or your own JSONL cases:

```bash
go run ./cmd/babel-eval --live pipeline \
  --endpoint http://127.0.0.1:8080 \
  --profile default \
  --limit 10 --max-requests 20 \
  --output eval/results/live.jsonl
go run ./cmd/babel-eval report eval/results/live.jsonl -output eval/results/live.html
```

See [eval/README.md](eval/README.md) for all flags and reporting options.

## Documentation

Guides in Chinese: [Domain library](docs/usage/domain-library.md) · [Token counting](docs/usage/token-counting.md) · [Design](docs/design/semantic-compression.md) · [Transport security](docs/security/transparent-proxy-review.md) · [Roadmap](docs/plans/ROADMAP.md). License: [MIT](LICENSE).
