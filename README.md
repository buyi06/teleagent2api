# TeleAgent2API

OpenAI-Compatible API Gateway for TeleAgent (星辰超级智能体)

---

## Features

- Pure Go, single binary, minimal Docker image
- Full OpenAI Chat Completions compatibility (streaming & non-streaming)
- Tool calls / function calling support
- Built-in API Key authentication
- Request sanitization — strips unsupported params (logprobs, stop, n, etc.) that cause upstream errors
- Response adapter - transforms upstream responses to OpenAI-compatible format
  - Preserves `reasoning_content` for clients that support reasoning streams
  - Cleans `usage` to only standard fields
  - Streaming: buffers until first useful chunk so empty streams can be retried; preserves tool-call / finish chunks
- Model metadata with context limits and max output tokens
- Automatic `max_tokens` cap per model
- Upstream/network retry and extra empty-response retry with configurable counts
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
| `TELEAGENT_TIMEOUT` | `20m` | Upstream response-header timeout / long-turn headroom |
| `TELEAGENT_CHUNK_TIMEOUT` | `15m` | Maximum idle time while reading an upstream response chunk |
| `TELEAGENT_LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `TELEAGENT_LOG_FORMAT` | `text` | Log format (text/json) |
| `TELEAGENT_RETRY_COUNT` | `0` | Retry count on upstream/network errors |
| `TELEAGENT_EMPTY_RETRY_COUNT` | `2` | Extra retries for empty non-streaming or header-only streaming responses |

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
5. Streaming: empty/header-only streams are retried before headers are committed; useful reasoning/tool/finish chunks are preserved

## License

MIT
