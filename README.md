<h1 align="center">🚀 TeleAgent2API</h1>
<h3 align="center">OpenAI-Compatible API Gateway for TeleAgent</h3>

---

## 📖 简介

TeleAgent2API 是一个轻量级的 API 网关，将 TeleAgent 服务转换为标准的 OpenAI Chat Completions 接口。

- ⚡ 纯 Go 实现，Docker 镜像极小
- 🔐 内置 API Key 认证保护
- 🌊 支持流式 (SSE) 和非流式响应
- 🎯 兼容任何 OpenAI API 客户端
- 📄 支持通过 `config.json` 或环境变量进行配置

---

##  ⚠️ 已知问题
在接入Claude Code使用时，有时会出现 "API 调用参数有误，请检查文档" 报错，重试后正常


## 🚀 快速开始

### Docker（推荐）

1. 克隆代码或下载：
```bash
git clone <repository_url>
cd teleagent2api
```

2. 复制环境变量模板并填入参数：
```bash
cp .env.example .env
# 编辑 .env 文件填入您的 TELEAGENT_TOKEN 、DEVICE_ID 和 INSTALL_ID 
```

3. 启动：
```bash
docker compose up -d --build
```

### 源码编译

```bash
go mod tidy
go build -o teleagent2api .
```

直接运行前，准备 `config.json` 或设置环境变量：
```bash
cp config.example.json config.json
./teleagent2api
```

### 验证

```bash
# 健康检查
curl http://localhost:10000/health

# 模型列表
curl http://localhost:10000/v1/models \
  -H "Authorization: Bearer sk-custom-your-key"

# 聊天请求
curl -N http://localhost:10000/v1/chat/completions \
  -H "Authorization: Bearer sk-custom-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat-flash","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

---

## ⚙️ 配置

支持环境变量和 `config.json` 两种方式。环境变量优先级高于配置文件（12-factor 合规）。默认读取 `config.json`，可通过 `TELEAGENT_CONFIG` 变量指定路径。

| 环境变量 / JSON 键 | 默认值 | 说明 |
|---------|--------|------|
| `TELEAGENT_TOKEN` / `token` | 空 | (必填) 您的 TeleAgent JWT Token |
| `TELEAGENT_INSTALL_ID` / `installId` | 空 | (必填) 安装 ID |
| `TELEAGENT_DEVICE_ID` / `deviceId` | 空 | (必填) 设备 ID |
| `API_KEY` / `apiKey` | 空 | 网关对外的 API Key，为空则不鉴权 |
| `TELEAGENT2API_LISTEN` / `listen` | `:10000` | 监听地址 |
| `TELEAGENT_UPSTREAM_KEY` / `upstreamApiKey` | (内置) | 上游 API Key，一般无需修改 |
| `TELEAGENT_BASE_URL` / `baseURL` | `https://agent.teleai.com.cn` | 上游接口地址 |
| `TELEAGENT_APP_VERSION` / `appVersion` | `2.0.0` | 客户端版本号 |
| `TELEAGENT_USER_AGENT` / `userAgent` | (内置) | User-Agent |
| `TELEAGENT_MODELS` / `models` | `chat-lite,chat-pro,chat-flash` | 可用模型列表 |
| `TELEAGENT_TIMEOUT` / `timeout` | `120s` | 请求超时 |
| `TELEAGENT_LOG_LEVEL` / `logLevel` | `info` | 日志级别 (debug/info/warn/error) |
| `TELEAGENT_LOG_FORMAT` / `logFormat` | `text` | 日志格式 (text/json) |
| `TELEAGENT_RETRY_COUNT` / `retryCount` | `0` | 上游 5xx 重试次数 |

---

##  ❓ 如何获取令牌信息

1. 自行安装官方客户端并登录后找到 "~\AppData\Roaming\TeleAgent\app-auth" 目录中的 **state.json**  文件
2. 该文件中拥有所需的 **'token(**此令牌有效期为一个月**)'**、**'deviceId'**、**'installId'** 


## ⚠️ 免责声明

- 本项目仅供学习和研究使用，不得用于任何商业用途或牟利。
- 本项目不提供任何模型服务，仅作为接口转换网关使用。
