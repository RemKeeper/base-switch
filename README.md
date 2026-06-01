# 🤖 AI Reverse Proxy (baseSwitch)

多 AI Provider 聚合反向代理服务，提供统一的 OpenAI 兼容 API 入口，自动将请求路由到后端多个 AI 供应商。

## ✨ 特性

- **多 Provider 聚合** — 支持同时接入多个 AI 供应商，统一对外暴露 `/v1/models` 和 `/v1/chat/completions`
- **OpenAI 兼容** — 请求和响应格式完全兼容 OpenAI API，无缝替换
- **自动模型发现** — models 为空的 Provider 启动时自动调用 `/v1/models` 发现可用模型
- **流式响应支持** — 完整支持 SSE (Server-Sent Events) 流式输出
- **前置 API Key 鉴权** — 可选的 Bearer Token 鉴权，保护代理入口
- **管理 API** — 通过独立的 API Key 在线管理 Provider，支持增删改查
- **Chrome 指纹伪装** — 基于 `req/v3` 的 Chrome 指纹模拟，降低被上游拦截风险
- **SQLite 持久化** — Provider 配置自动存入 SQLite，重启不丢失
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
| `providers` | []object | AI Provider 列表 |

### Provider 配置

```json
{
  "name": "my-provider",
  "base_url": "https://api.example.com",
  "api_key": "sk-xxxxxxxx",
  "models": ["gpt-4o", "gpt-4o-mini"],
  "enabled": true
}
```

| 字段 | 说明 |
|---|---|
| `name` | Provider 唯一标识，用于模型路由 |
| `base_url` | Provider API 基础地址 |
| `api_key` | Provider API Key |
| `models` | 模型列表，为空时自动发现 |
| `enabled` | 是否启用 |

## 🔌 API 端点

### 代理端点（需代理 API Key）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/health` | 健康检查（无需鉴权） |
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
    ├── proxy/
    │   └── handler.go           # HTTP 代理处理器、路由与鉴权
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
