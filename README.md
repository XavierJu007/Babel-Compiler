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

Requires **Go 1.22+** and an OpenAI Chat Completions-compatible service for the compiler model.

```bash
git clone https://github.com/XavierJu007/Babel-Compiler.git
cd Babel-Compiler
go build -o bin/babel ./babel-go
./bin/babel
```

Babel selects an available port in `127.0.0.1:8080–8099` and tries to open your browser. Save the request URL, API key and model name in the model settings, then paste a prompt to compile. Create your own dictionaries in the domain library.

**Complete endpoints receive POST requests unchanged.** Host-only URLs and base URLs ending in `/v1` get `/chat/completions` appended for compatibility. Remote services require HTTPS; HTTP is allowed only on loopback. [Endpoint and validation guide (Chinese)](docs/usage/validation.md).

## CLI and API

```bash
./bin/babel -gate "Your prompt"
./bin/babel -gate -q -f prompt.md > compiled.txt
```

Start a fixed port with `./bin/babel -serve 127.0.0.1:8080`, then call the API from another terminal:

```bash
curl http://127.0.0.1:8080/api/compile \
  -H 'Content-Type: application/json' \
  -d '{"text":"Your prompt","strict_gate":true}'
```

## Transparent proxy

Set the final service's base URL as `upstream_url` in the model settings, then point your client at the corresponding local endpoint:

| Protocol | Endpoint | Upstream authentication supplied by the caller |
| --- | --- | --- |
| OpenAI | `/v1/chat/completions` | `Authorization` |
| Anthropic | `/v1/messages` | `x-api-key` |

**Compilation uses the saved profile; final execution uses `model` from the request body.** The caller must provide the upstream key; the proxy never borrows the compiler key.

The proxy compresses `system`, `user` and tool descriptions, replacing only their text while preserving message roles and tool structure. Model responses pass through without translation. [Full setup and fallback rules (Chinese)](docs/usage/validation.md#4-代理使用).

## Documentation

Guides in Chinese: [Domain library](docs/usage/domain-library.md) · [Token counting](docs/usage/token-counting.md) · [Design](docs/design/semantic-compression.md) · [Transport security](docs/security/transparent-proxy-review.md) · [Roadmap](docs/plans/ROADMAP.md). License: [MIT](LICENSE).
