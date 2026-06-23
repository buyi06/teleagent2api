# TeleAgent2API

OpenAI-Compatible API Gateway for TeleAgent (星辰超级智能体)

---

## Features

- Pure Go, single binary, minimal Docker image
- Full OpenAI Chat Completions compatibility (streaming & non-streaming)
- Tool calls / function calling support
- Built-in API Key authentication
- Request sanitization — strips unsupported params (logprobs, stop, n, etc.) that cause upstream errors
- Response adapter — transforms upstream responses to strict OpenAI format
  - Configurable reasoning handling (see [Reasoning Modes](#reasoning-modes)) — the upstream emits its
    chain-of-thought as `<think>...</think>` inside `content`; the gateway can re-route it to
    `reasoning_content` (OpenAI o1 style / Anthropic `thinking`), keep it inline, or drop it
  - Cleans `usage` to only standard fields
  - Streaming: emits `role` only in the first delta, drops empty separator chunks
- Model metadata with context limits and max output tokens
- Automatic `max_tokens` cap per model
- 5xx retry with configurable count
- Streaming progress diagnostics log
- Configurable via environment variables or `config.json`

## Models

| Model | Name | Context | Max Output | Tool Call | Reasoning |
|-------|------|---------|------------|-----------|-----------|
| `chat-lite` | 轻量 | 100K | 16,384 | Yes | No |
| `chat-pro` | 旗舰 | 192K | 65,536 | Yes | Yes |
| `chat-flash` | 极速 | 192K | 65,536 | Yes | No |

## Quick Start

### Docker (recommended)

```bash
git clone https://github.com/buyi06/teleagent2api.git
cd teleagent2api
```

Create `.env`:

```bash
TELEAGENT_TOKEN=your_jwt_token
TELEAGENT_DEVICE_ID=your_device_id
TELEAGENT_INSTALL_ID=your_install_id
API_KEY=sk-your-custom-key
```

```bash
docker compose up -d --build
```

### From Source

```bash
go mod tidy
go build -o teleagent2api .
cp config.example.json config.json
# edit config.json with your credentials
./teleagent2api
```

### Verify

```bash
# Health
curl http://localhost:10000/health

# Models (with metadata)
curl http://localhost:10000/v1/models \
  -H "Authorization: Bearer sk-your-custom-key"

# Chat (non-streaming)
curl http://localhost:10000/v1/chat/completions \
  -H "Authorization: Bearer sk-your-custom-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat-flash",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'

# Chat (streaming)
curl -N http://localhost:10000/v1/chat/completions \
  -H "Authorization: Bearer sk-your-custom-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat-flash",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'

# Tool calls
curl http://localhost:10000/v1/chat/completions \
  -H "Authorization: Bearer sk-your-custom-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat-flash",
    "messages": [{"role": "user", "content": "weather in Tokyo?"}],
    "stream": false,
    "tools": [{"type": "function", "function": {
      "name": "get_weather",
      "description": "Get weather for a city",
      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
    }}]
  }'
```

## Claude Code Integration

```bash
export OPENAI_API_KEY=sk-your-custom-key
export OPENAI_BASE_URL=http://your-host:10000/v1
claude
```

Or in `.claude/settings.json`:

```json
{
  "env": {
    "OPENAI_API_KEY": "sk-your-custom-key",
    "OPENAI_BASE_URL": "http://your-host:10000/v1"
  }
}
```

> Tip: `chat-pro` is a reasoning model. Run it with `TELEAGENT_REASONING_MODE=reasoning_content`
> so the long thinking phase streams as `reasoning_content` (rendered as a live "thinking" block by
> reasoning-aware clients / gateways) instead of the client appearing frozen while reasoning is hidden.

## Configuration

Environment variables take precedence over `config.json`.

| Variable | Default | Description |
|----------|---------|-------------|
| `TELEAGENT_TOKEN` | — | **(required)** JWT token from TeleAgent |
| `TELEAGENT_DEVICE_ID` | — | **(required)** Device ID |
| `TELEAGENT_INSTALL_ID` | — | **(required)** Install ID |
| `API_KEY` | — | Gateway API Key (empty = no auth) |
| `TELEAGENT2API_LISTEN` | `:10000` | Listen address |
| `TELEAGENT_UPSTREAM_KEY` | (built-in) | Upstream API Key |
| `TELEAGENT_BASE_URL` | `https://agent.teleai.com.cn` | Upstream base URL |
| `TELEAGENT_APP_VERSION` | `2.0.0` | Client version header |
| `TELEAGENT_USER_AGENT` | (built-in) | User-Agent header |
| `TELEAGENT_MODELS` | `chat-lite,chat-pro,chat-flash` | Available models |
| `TELEAGENT_TIMEOUT` | `300s` | Request timeout (covers the whole stream — keep large for long reasoning) |
| `TELEAGENT_LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `TELEAGENT_LOG_FORMAT` | `text` | Log format (text/json) |
| `TELEAGENT_RETRY_COUNT` | `0` | Retry count on upstream 5xx |
| `TELEAGENT_REASONING_MODE` | `content` | How `<think>` reasoning is delivered (see below) |
| `TELEAGENT_STREAM_LOG_EVERY` | `5s` | Interval for the `stream progress` diagnostics log (`0` disables) |

## Reasoning Modes

The TeleAgent upstream (GLM-family) emits its chain-of-thought as a `<think>...</think>` block at the
**start of the `content` stream**, followed by the real answer. For a complex prompt the thinking phase
can last minutes. If a downstream client/gateway hides or buffers that `<think>` block, the user sees a
frozen spinner with no visible progress until the answer finally appears.

`TELEAGENT_REASONING_MODE` controls how the gateway delivers that reasoning (the `<think>` tags are
matched across chunk boundaries, so partial tags split between SSE frames are handled correctly):

| Mode | Behavior | Use when |
|------|----------|----------|
| `content` (default) | Pass through unchanged — `<think>` stays inside `content` | Client already renders `<think>` tags |
| `reasoning_content` | Split the `<think>` block into `delta.reasoning_content`; the answer stays in `content`; tags stripped | **Recommended.** Reasoning-aware clients (OpenAI o1) and gateways that map `reasoning_content` → Anthropic `thinking` show a live thinking block, answer stays clean |
| `visible` | Strip the `<think>` tags and stream the reasoning as plain `content` | Client has no reasoning channel but you still want continuous output (reasoning mixes into the answer) |
| `strip` | Drop the reasoning entirely; stream only the answer | You only want the final answer |

Applies to both streaming and non-streaming responses.

## Getting Credentials

1. Install [TeleAgent](https://www.teleai.cn/) client and log in
2. Find `state.json` at:
   - **Windows:** `%APPDATA%\TeleAgent\app-auth\state.json`
   - **macOS:** `~/Library/Application Support/TeleAgent/app-auth/state.json`
   - **Linux:** `~/.config/TeleAgent/app-auth/state.json`
3. Extract `token`, `deviceId`, and `installId`

```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "deviceId": "a1b2c3d4e5f6",
  "installId": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
}
```

> Note: `token` expires after one month. Re-login to refresh.

## Architecture

```
Client (Claude Code / any OpenAI client)
  │
  ├── /health
  ├── /v1/models
  └── /v1/chat/completions
        │
        ▼
  ┌─────────────────────────────┐
  │   TeleAgent2API Gateway     │
  │                             │
  │  Auth → Sanitize Request    │
  │       → HMAC Sign           │
  │       → Forward to Upstream │
  │       → Transform Response  │
  │       → Return to Client    │
  └─────────────────────────────┘
        │
        ▼
  agent.teleai.com.cn
  (TeleAgent Cloud API)
```

**Request flow:**
1. Auth middleware validates API Key
2. Adapter sanitizes request (strips unsupported OpenAI params, caps `max_tokens`)
3. Proxy builds upstream request with HMAC signature
4. Upstream response is transformed to strict OpenAI format
5. Streaming: reasoning is routed per `TELEAGENT_REASONING_MODE`, `role` emitted only once

## License

MIT
