# 🤖 AI Reverse Proxy (baseSwitch)

多 AI Provider 聚合反向代理服务，提供统一的 OpenAI 兼容 API 入口，自动将请求路由到后端多个 AI 供应商。

## ✨ 特性

- **多 Provider 聚合** — 支持同时接入多个 AI 供应商，统一对外暴露 `/v1/models` 和 `/v1/chat/completions`
- **OpenAI 兼容** — 请求和响应格式完全兼容 OpenAI API，无缝替换
- **自动模型发现** — models 为空的 Provider 启动时自动调用 `/v1/models` 发现可用模型
- **自动模型刷新** — 可配置固定时间间隔，定时刷新所有已启用 Provider 的模型列表
- **流式响应支持** — 完整支持 SSE (Server-Sent Events) 流式输出
- **前置 API Key 鉴权** — 可选的 Bearer Token 鉴权，保护代理入口
- **管理 API** — 通过独立的 API Key 在线管理 Provider，支持增删改查
- **Worker Pages 管理面板** — `worker-pages/` 提供可直接部署的静态管理页
- **模型存活检测** — 通过管理 API 批量检测各 Provider 模型是否可用
- **Token 用量统计** — 插件式统计各 Provider / 模型的请求次数和 token 消耗
- **路由分组与自动重试** — 将分组名作为模型名，随机选择下级 Provider/Model；可选失败后最多切换 3 个其他成员重试
- **Gin 路由管理** — 基于 Gin Router 进行路由分组、中间件鉴权和 CORS 管理
- **Chrome 指纹伪装** — 基于 `req/v3` 的 Chrome 指纹模拟，降低被上游拦截风险
- **SQLite + GORM 持久化** — Provider 配置与 Token 用量统计基于 GORM + SQLite 持久化，重启不丢失
- **优雅关闭** — 支持 SIGINT / SIGTERM 信号优雅退出

## 📦 快速开始

### 前置要求

- Go 1.25+

### 安装与运行

```bash
# 克隆仓库
git clone <repo-url> baseSwitch
cd baseSwitch

# 复制并编辑配置
cp config.example.json config.json
# 编辑 config.json，填入你的 Provider 信息

# 安装依赖并运行
go mod tidy
go run main.go
```

### 使用预编译二进制

```bash
# 构建
go build -o baseSwitch .

# 运行
./baseSwitch -config config.json
```

## ⚙️ 配置说明

配置文件为 `config.json`，结构如下：

| 字段 | 类型 | 说明 |
|---|---|---|
| `listen.host` | string | 监听地址，默认 `0.0.0.0` |
| `listen.port` | int | 监听端口，默认 `28080` |
| `database.path` | string | SQLite 数据库路径 |
| `auth.enabled` | bool | 是否启用前置 API Key 鉴权 |
| `auth.keys` | []string | 允许的代理 API Key 列表 |
| `management.enabled` | bool | 是否启用管理 API |
| `management.keys` | []string | 管理 API Key 列表（独立于代理鉴权） |
| `model_refresh.enabled` | bool | 是否启用 Provider 模型列表自动刷新 |
| `model_refresh.interval_seconds` | int | 自动刷新间隔，单位秒，默认 `3600` |
| `providers` | []object | AI Provider 列表 |
| `route_groups` | []object | 路由分组列表 |

### 路由分组

请求模型名称等于分组 `name` 时，系统会从 `members` 中随机选择一个 Provider/Model。启用 `auto_retry` 后，首选 Provider 返回失败或请求错误时，最多再尝试 3 个其他分组成员；流式响应仅在上游尚未开始返回内容时切换。

```json
{
  "route_groups": [
    {
      "name": "smart-model",
      "members": [
        { "provider": "example-provider", "model": "gpt-4o" },
        { "provider": "backup-provider", "model": "gpt-4o-mini" }
      ],
      "auto_retry": true
    }
  ]
}
```

也可通过管理 API 管理：`GET/POST /admin/route-groups`、`PUT/DELETE /admin/route-groups/:name`。

### 模型自动刷新配置

```json
{
  "model_refresh": {
    "enabled": true,
    "interval_seconds": 3600
  }
}
```

启用后，服务会按 `interval_seconds` 指定的间隔，定时调用所有已启用 Provider 的 `/v1/models`，并将最新模型列表写入 SQLite。刷新失败的 Provider 会被跳过，不影响其他 Provider 和代理服务。

### Provider 配置

```json
{
  "name": "my-provider",
  "api_type": "openai",
  "base_url": "https://api.example.com",
  "api_key": "sk-xxxxxxxx",
  "proxy_url": "http://127.0.0.1:7890",
  "models": ["gpt-4o", "gpt-4o-mini"],
  "enabled": true
}
```

| 字段 | 说明 |
|---|---|
| `name` | Provider 唯一标识，用于模型路由 |
| `api_type` | 上游 API 协议，支持 `openai` 或 `anthropic`，默认 `openai` |
| `base_url` | Provider API 基础地址 |
| `api_key` | Provider API Key |
| `proxy_url` | 可选，上游请求代理地址；为空则直连 |
| `models` | 模型列表，为空时启动或通过管理 API 新增 Provider 时自动发现 |
| `enabled` | 是否启用 |

模型存活检测会根据 `api_type` 选择上游接口：OpenAI 使用 `/v1/chat/completions`，Anthropic 使用 `/v1/messages`，并自动设置对应鉴权请求头。

## 🔌 API 端点

### 代理端点（需代理 API Key）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查（无需鉴权） |
| `GET` | `/doc.json` | OpenAPI 3.1 文档，仅允许 `127.0.0.1` 访问 |
| `GET` | `/v1/models` | 获取所有可用模型列表 |
| `POST` | `/v1/chat/completions` | Chat Completions（OpenAI 兼容） |
| `*` | `/v1/*` | 通用代理转发 |

### 管理端点（需管理 API Key）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/admin/providers` | 列出所有 Provider |
| `POST` | `/admin/providers` | 新增 Provider |
| `GET` | `/admin/providers/:name` | 获取指定 Provider |
| `PUT` | `/admin/providers/:name` | 更新指定 Provider |
| `DELETE` | `/admin/providers/:name` | 删除指定 Provider |
| `POST` | `/admin/providers/:name/refresh-models` | 手动刷新指定 Provider 模型列表 |
| `POST` | `/admin/models/check` | 流式检测模型存活状态，返回 NDJSON |
| `GET` | `/admin/usage/summary` | 查询 token 消耗聚合统计 |
| `GET` | `/admin/usage/records` | 查询 token 消耗明细记录 |

### Worker Pages 管理面板

项目内置纯静态管理页，可部署到 Cloudflare Pages / Workers Pages：

1. 在 Cloudflare Pages 中创建项目。
2. 构建命令留空。
3. 输出目录填写 `worker-pages`。
4. 部署后打开页面，填写 baseSwitch 后端地址和管理 API Key。

页面支持 Provider 增删改查、Token 用量统计和模型存活检测。管理 API Key 保存在当前浏览器的 `localStorage`。

Provider 列表中的“刷新模型”按钮会通过指定 Provider 的 `/v1/models` 接口重新获取模型列表，并保存到数据库；请求会沿用该 Provider 配置的代理地址。

### 模型存活检测

模型存活检测会对目标上游 `/v1/chat/completions` 发送最小非流式请求，可能产生少量 token 消耗。接口按顺序逐个检测模型，每完成一个模型立即返回一行 NDJSON，避免某个模型失败导致前面已完成的结果无法获取，也避免并发触发上游 RPM 限制。

```bash
curl -X POST http://localhost:28080/admin/models/check \
  -H "Authorization: Bearer sk-admin-management-key" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "example-provider",
    "models": ["example-provider/gpt-4o"],
    "timeout_seconds": 20
  }'
```

### Token 用量统计

Token 用量统计以插件形式实现，数据单独存储在 `./data/token_usage.db`，与 Provider 主存储分离。

当前会从 OpenAI 兼容响应中的 `usage` 字段读取：

- `prompt_tokens`
- `completion_tokens`
- `total_tokens`

非流式响应直接解析 JSON；流式响应会尝试解析 SSE 中包含 `usage` 的 chunk（需要上游支持 `stream_options.include_usage`）。若未返回 usage，会记录一次 0 token 请求，用于统计请求次数。

```bash
# 按 Provider + Model 查询聚合统计
curl "http://localhost:28080/admin/usage/summary?group_by=model" \
  -H "Authorization: Bearer sk-admin-management-key"

# 仅按 Provider 聚合
curl "http://localhost:28080/admin/usage/summary?group_by=provider" \
  -H "Authorization: Bearer sk-admin-management-key"

# 查询最近 100 条明细
curl "http://localhost:28080/admin/usage/records?limit=100" \
  -H "Authorization: Bearer sk-admin-management-key"
```

### 模型路由格式

请求时 `model` 字段支持两种格式：

- **明确指定 Provider**：`Provider名称/模型名`，如 `openai/gpt-4o`
- **仅模型名**：`gpt-4o`，系统自动搜索所有 Provider 中匹配的模型

## 🔐 鉴权体系

项目采用**双层鉴权**，两组 Key 完全独立：

| 层级 | 配置路径 | 用途 |
|---|---|---|
| 代理鉴权 | `auth.keys` | 保护 `/v1/*` 代理端点 |
| 管理鉴权 | `management.keys` | 保护 `/admin/*` 管理端点 |

鉴权方式：`Authorization: Bearer <your-key>`

## 📋 使用示例

### 代理调用

```bash
# 健康检查
curl http://localhost:28080/health

# 获取模型列表
curl http://localhost:28080/v1/models \
  -H "Authorization: Bearer sk-your-proxy-key-here"

# Chat Completions
curl http://localhost:28080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-proxy-key-here" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'
```

### 管理 Provider

```bash
# 新增 Provider
curl -X POST http://localhost:28080/admin/providers \
  -H "Authorization: Bearer sk-admin-management-key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "new-provider",
    "base_url": "https://api.new-provider.com",
    "api_key": "sk-xxxxxxxx",
    "models": ["gpt-4o"],
    "enabled": true
  }'

# 新增 Provider 并自动发现模型（models 传空数组）
curl -X POST http://localhost:28080/admin/providers \
  -H "Authorization: Bearer sk-admin-management-key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "auto-discover-provider",
    "base_url": "https://api.example.com",
    "api_key": "sk-xxxxxxxx",
    "models": [],
    "enabled": true
  }'

# 列出所有 Provider
curl http://localhost:28080/admin/providers \
  -H "Authorization: Bearer sk-admin-management-key"

# 更新 Provider
curl -X PUT http://localhost:28080/admin/providers/new-provider \
  -H "Authorization: Bearer sk-admin-management-key" \
  -H "Content-Type: application/json" \
  -d '{
    "base_url": "https://api.new-provider.com",
    "api_key": "sk-yyyyyyyy",
    "models": ["gpt-4o", "claude-3"],
    "enabled": false
  }'

# 删除 Provider
curl -X DELETE http://localhost:28080/admin/providers/new-provider \
  -H "Authorization: Bearer sk-admin-management-key"
```

## 🗂️ 项目结构

```
baseSwitch/
├── main.go                      # 入口，服务启动与优雅关闭
├── config.json                  # 运行配置（不提交 Git）
├── config.example.json          # 示例配置
├── go.mod / go.sum              # Go 模块依赖
├── data/
│   └── providers.db             # SQLite 数据库（自动生成）
└── internal/
    ├── config/
    │   └── config.go            # 配置加载与结构定义
    ├── provider/
    │   └── manager.go           # Provider 管理器
    ├── plugins/
    │   └── tokenusage/           # Token 用量统计插件
    ├── proxy/
    │   └── handler.go           # Gin 路由、HTTP 代理处理器与鉴权中间件
    └── storage/
        └── storage.go           # SQLite 存储层、模型缓存
```

## 🛡️ 安全建议

1. **务必修改默认密钥** — `config.json` 中 `auth.keys` 和 `management.keys` 的默认值仅用于开发测试
2. **config.json 不要提交到 Git** — 项目 `.gitignore` 已排除 `config.json`，请使用 `config.example.json` 作为模板
3. **生产环境启用鉴权** — 将 `auth.enabled` 设为 `true` 并设置强随机 Key
4. **管理 API 限制访问** — 建议通过防火墙或反向代理限制 `/admin/*` 的访问来源

## 📄 License

MIT
