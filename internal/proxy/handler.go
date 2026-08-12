package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"baseSwitch/internal/config"
	"baseSwitch/internal/plugins/tokenusage"
	"baseSwitch/internal/provider"
	"baseSwitch/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
)

// Handler 反向代理处理器
type Handler struct {
	manager  *provider.Manager
	client   *req.Client
	auth     *config.AuthConfig       // 前置鉴权配置
	mgmtAuth *config.ManagementConfig // 管理 API 鉴权配置
	usage    *tokenusage.Plugin
	router   *gin.Engine
}

// NewHandler 创建代理处理器，启用 Chrome 指纹伪装
func NewHandler(mgr *provider.Manager, auth *config.AuthConfig, mgmtAuth *config.ManagementConfig, usage *tokenusage.Plugin) *Handler {
	client := req.C().
		ImpersonateChrome().
		SetTimeout(5*time.Minute).
		SetCommonHeader("Content-Type", "application/json").
		SetCommonHeader("Accept", "application/json, text/event-stream, */*")

	client.GetClient().Transport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	h := &Handler{manager: mgr, client: client, auth: auth, mgmtAuth: mgmtAuth, usage: usage}
	h.router = h.buildRouter()
	return h
}

// Router 返回 Gin 路由实例
func (h *Handler) Router() *gin.Engine { return h.router }

// ServeHTTP 兼容 http.Handler
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.router.ServeHTTP(w, r) }

// buildRouter 构建 Gin 路由树
func (h *Handler) buildRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), corsMiddleware())

	r.GET("/health", h.handleHealth)
	r.GET("/doc.json", localOnlyMiddleware(), h.handleOpenAPIDoc)

	admin := r.Group("/admin", h.managementAuthMiddleware())
	{
		providers := admin.Group("/providers")
		providers.GET("", h.handleAdminListProviders)
		providers.POST("", h.handleAdminAddProvider)
		providers.GET("/:name", h.handleAdminGetProvider)
		providers.PUT("/:name", h.handleAdminUpdateProvider)
		providers.DELETE("/:name", h.handleAdminDeleteProvider)
		providers.POST("/:name/refresh-models", h.handleAdminRefreshProviderModels)

		usage := admin.Group("/usage")
		usage.GET("/summary", h.handleAdminUsageSummary)
		usage.GET("/records", h.handleAdminUsageRecords)
	}
	models := admin.Group("/models")
	models.POST("/check", h.handleAdminCheckModels)

	v1 := r.Group("/v1", h.proxyAuthMiddleware())
	{
		v1.GET("/models", h.HandleModels)
		v1.POST("/chat/completions", h.HandleChatCompletions)
	}

	r.NoRoute(h.handleNoRoute)

	return r
}

// handleNoRoute 处理 Gin 未命中的通用代理路由
func (h *Handler) handleNoRoute(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/v1/") && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodPost) {
		h.proxyAuthMiddleware()(c)
		if c.IsAborted() {
			return
		}
		h.handleGenericProxy(c)
		return
	}

	c.JSON(http.StatusNotFound, errorResponse("路由不存在"))
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusOK)
			return
		}
		c.Next()
	}
}

func localOnlyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		remoteIP := c.ClientIP()
		if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
			remoteIP = host
		}

		if remoteIP != "127.0.0.1" {
			c.AbortWithStatusJSON(http.StatusForbidden, errorResponse("仅允许 127.0.0.1 访问该文档路由"))
			return
		}

		c.Next()
	}
}

func (h *Handler) proxyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.auth == nil || !h.auth.IsAuthEnabled() {
			c.Next()
			return
		}
		key := extractBearerToken(c.GetHeader("Authorization"))
		if key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "缺少 Authorization header，请提供 Bearer API Key", "type": "authentication_error"}})
			return
		}
		if !h.auth.IsValidKey(key) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "无效的 API Key，拒绝访问", "type": "authentication_error"}})
			return
		}
		c.Next()
	}
}

func (h *Handler) managementAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.mgmtAuth == nil || !h.mgmtAuth.IsManagementEnabled() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "管理 API 未启用", "type": "admin_disabled"}})
			return
		}
		key := extractBearerToken(c.GetHeader("Authorization"))
		if key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "缺少 Authorization header，请提供管理 API Key", "type": "authentication_error"}})
			return
		}
		if !h.mgmtAuth.IsValidManagementKey(key) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "无效的管理 API Key，拒绝访问", "type": "authentication_error"}})
			return
		}
		c.Next()
	}
}

func extractBearerToken(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	key := strings.TrimPrefix(authHeader, "Bearer ")
	if key == authHeader || key == "" {
		key = authHeader
	}
	return key
}

type ChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream,omitempty"`
	Messages []any  `json:"messages,omitempty"`
}
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
type ModelListResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

func (h *Handler) HandleModels(c *gin.Context) {
	entries, err := h.manager.GetAllModels()
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取模型列表失败: "+err.Error()))
		return
	}
	data := make([]ModelEntry, 0, len(entries))
	for _, e := range entries {
		data = append(data, ModelEntry{ID: e.ID, Object: e.Object, Created: e.Created, OwnedBy: e.OwnedBy})
	}
	c.JSON(http.StatusOK, ModelListResponse{Object: "list", Data: data})
}

func (h *Handler) HandleChatCompletions(c *gin.Context) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorResponse("读取请求体失败"))
		return
	}
	defer c.Request.Body.Close()
	var chatReq ChatRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse("解析请求体失败"))
		return
	}
	if chatReq.Model == "" {
		c.JSON(http.StatusBadRequest, errorResponse("model 字段不能为空"))
		return
	}
	prov, actualModel, err := h.manager.ResolveModel(chatReq.Model)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	log.Printf("[PROXY] %s -> %s/%s (stream=%v)", chatReq.Model, prov.Name, actualModel, chatReq.Stream)
	newBody := h.rewriteModelField(bodyBytes, actualModel)
	if chatReq.Stream {
		newBody = h.ensureStreamUsageOption(newBody)
	}
	targetURL := strings.TrimRight(prov.BaseURL, "/") + "/v1/chat/completions"
	if chatReq.Stream {
		h.forwardStream(c.Writer, prov.Name, actualModel, c.Request.URL.Path, prov.APIKey, prov.ProxyURL, targetURL, newBody)
		return
	}
	h.forwardNonStream(c.Writer, prov.Name, actualModel, c.Request.URL.Path, prov.APIKey, prov.ProxyURL, targetURL, newBody)
}

func (h *Handler) handleGenericProxy(c *gin.Context) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorResponse("读取请求体失败"))
		return
	}
	defer c.Request.Body.Close()
	var raw map[string]any
	modelStr := ""
	if json.Unmarshal(bodyBytes, &raw) == nil {
		if m, ok := raw["model"].(string); ok {
			modelStr = m
		}
	}
	if modelStr == "" {
		c.JSON(http.StatusBadRequest, errorResponse("无法确定目标 provider，请使用 Provider/model 格式指定模型"))
		return
	}
	prov, actualModel, err := h.manager.ResolveModel(modelStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	newBody := h.rewriteModelField(bodyBytes, actualModel)
	targetURL := strings.TrimRight(prov.BaseURL, "/") + c.Request.URL.Path
	log.Printf("[PROXY] %s %s -> %s/%s", c.Request.Method, c.Request.URL.Path, prov.Name, actualModel)
	reqBuilder := h.providerClient(prov.ProxyURL).R().SetHeader("Authorization", "Bearer "+prov.APIKey)
	if len(newBody) > 0 {
		reqBuilder.SetBodyJsonBytes(newBody)
	}
	var resp *req.Response
	if c.Request.Method == http.MethodGet {
		resp, err = reqBuilder.Get(targetURL)
	} else {
		resp, err = reqBuilder.Post(targetURL)
	}
	if err != nil {
		log.Printf("[ERROR] 转发请求失败: %v", err)
		c.JSON(http.StatusBadGateway, errorResponse("上游请求失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(c.Writer, resp.Header)
	c.Writer.WriteHeader(resp.StatusCode)
	body := resp.Bytes()
	c.Writer.Write(body)
	usage := tokenusage.ExtractUsageFromJSON(body)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		usage = tokenusage.ExtractUsageFromSSE(body)
	}
	h.recordTokenUsage(prov.Name, actualModel, c.Request.URL.Path, usage)
}

func (h *Handler) forwardNonStream(w http.ResponseWriter, providerName, modelName, endpoint, apiKey, proxyURL, targetURL string, body []byte) {
	resp, err := h.providerClient(proxyURL).R().SetHeader("Authorization", "Bearer "+apiKey).SetBodyJsonBytes(body).Post(targetURL)
	if err != nil {
		log.Printf("[ERROR] 转发请求失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"上游请求失败: %s"}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	respBody := resp.Bytes()
	w.Write(respBody)
	h.recordTokenUsage(providerName, modelName, endpoint, tokenusage.ExtractUsageFromJSON(respBody))
}

func (h *Handler) forwardStream(w http.ResponseWriter, providerName, modelName, endpoint, apiKey, proxyURL, targetURL string, body []byte) {
	resp, err := h.providerClient(proxyURL).R().SetHeader("Authorization", "Bearer "+apiKey).SetBodyJsonBytes(body).DisableAutoReadResponse().Post(targetURL)
	if err != nil {
		log.Printf("[ERROR] 转发流式请求失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"上游请求失败: %s"}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(w, resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("[ERROR] 不支持流式响应")
		return
	}
	buf := make([]byte, 4096)
	var capture []byte
	defer func() {
		h.recordTokenUsage(providerName, modelName, endpoint, tokenusage.ExtractUsageFromSSE(capture))
	}()
	for {
		n, readErr := resp.Response.Body.Read(buf)
		if n > 0 {
			capture = append(capture, buf[:n]...)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[ERROR] 读取流式响应失败: %v", readErr)
			}
			return
		}
	}
}

func (h *Handler) recordTokenUsage(providerName, modelName, endpoint string, usage *tokenusage.Usage) {
	if h.usage == nil {
		return
	}
	if err := h.usage.RecordUsage(providerName, modelName, endpoint, usage); err != nil {
		log.Printf("[WARN] token 统计写入失败: %v", err)
	}
}

func (h *Handler) rewriteModelField(body []byte, actualModel string) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	raw["model"] = actualModel
	newBody, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return newBody
}

func (h *Handler) ensureStreamUsageOption(body []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	streamOptions, ok := raw["stream_options"].(map[string]any)
	if !ok || streamOptions == nil {
		streamOptions = map[string]any{}
	}
	streamOptions["include_usage"] = true
	raw["stream_options"] = streamOptions

	newBody, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return newBody
}

func (h *Handler) handleHealth(c *gin.Context) {
	count, err := h.manager.GetProviderCount()
	if err != nil {
		count = -1
	}
	entries, _ := h.manager.GetAllModels()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "providers": count, "total_models": len(entries)})
}

func (h *Handler) handleOpenAPIDoc(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"openapi": "3.1.0",
		"info": gin.H{
			"title":       "baseSwitch AI Reverse Proxy API",
			"description": "多 Provider 聚合反向代理服务，提供 OpenAI 兼容代理 API、Provider 管理 API 和健康检查端点。",
			"version":     "1.0.0",
		},
		"servers": []gin.H{
			{"url": "/", "description": "当前服务地址"},
		},
		"tags": []gin.H{
			{"name": "System", "description": "系统状态与本地文档"},
			{"name": "OpenAI Proxy", "description": "OpenAI 兼容代理端点"},
			{"name": "Admin", "description": "Provider 管理端点"},
		},
		"paths": gin.H{
			"/health": gin.H{
				"get": gin.H{
					"tags":        []string{"System"},
					"summary":     "健康检查",
					"operationId": "getHealth",
					"responses": gin.H{
						"200": gin.H{"description": "服务状态", "content": jsonContent(schemaRef("#/components/schemas/HealthResponse"))},
					},
				},
			},
			"/doc.json": gin.H{
				"get": gin.H{
					"tags":        []string{"System"},
					"summary":     "OpenAPI 文档",
					"description": "仅允许从 127.0.0.1 访问。",
					"operationId": "getOpenAPIDoc",
					"responses": gin.H{
						"200": gin.H{"description": "OpenAPI 3.1 文档"},
						"403": gin.H{"description": "非本地访问被拒绝", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
					},
				},
			},
			"/v1/models": gin.H{
				"get": gin.H{
					"tags":        []string{"OpenAI Proxy"},
					"summary":     "获取所有聚合模型列表",
					"operationId": "listModels",
					"security":    []gin.H{{"ProxyBearerAuth": []string{}}},
					"responses": gin.H{
						"200": gin.H{"description": "模型列表", "content": jsonContent(schemaRef("#/components/schemas/ModelListResponse"))},
						"401": gin.H{"description": "代理 API Key 无效", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
					},
				},
			},
			"/v1/chat/completions": gin.H{
				"post": gin.H{
					"tags":        []string{"OpenAI Proxy"},
					"summary":     "Chat Completions（OpenAI 兼容）",
					"operationId": "createChatCompletion",
					"security":    []gin.H{{"ProxyBearerAuth": []string{}}},
					"requestBody": gin.H{"required": true, "content": jsonContent(schemaRef("#/components/schemas/ChatCompletionRequest"))},
					"responses": gin.H{
						"200": gin.H{"description": "上游 Provider 返回的 Chat Completions 响应或 SSE 流"},
						"400": gin.H{"description": "请求错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"401": gin.H{"description": "代理 API Key 无效", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"502": gin.H{"description": "上游请求失败", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
					},
				},
			},
			"/v1/{path}": gin.H{
				"get":  genericProxyOperation("GET 通用代理转发"),
				"post": genericProxyOperation("POST 通用代理转发"),
			},
			"/admin/providers": gin.H{
				"get": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "列出所有 Provider",
					"operationId": "listProviders",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"responses":   adminResponses(schemaRef("#/components/schemas/ProviderListResponse")),
				},
				"post": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "新增 Provider",
					"operationId": "createProvider",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"requestBody": gin.H{"required": true, "content": jsonContent(schemaRef("#/components/schemas/AdminProviderRequest"))},
					"responses":   adminResponses(schemaRef("#/components/schemas/MessageResponse")),
				},
			},
			"/admin/providers/{name}": gin.H{
				"parameters": []gin.H{pathParam("name", "Provider 名称")},
				"get": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "获取指定 Provider",
					"operationId": "getProvider",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"responses":   adminResponses(schemaRef("#/components/schemas/SafeProvider")),
				},
				"put": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "更新指定 Provider",
					"operationId": "updateProvider",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"requestBody": gin.H{"required": true, "content": jsonContent(schemaRef("#/components/schemas/AdminProviderRequest"))},
					"responses":   adminResponses(schemaRef("#/components/schemas/MessageResponse")),
				},
				"delete": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "删除指定 Provider",
					"operationId": "deleteProvider",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"responses":   adminResponses(schemaRef("#/components/schemas/MessageResponse")),
				},
			},
			"/admin/providers/{name}/refresh-models": gin.H{
				"parameters": []gin.H{pathParam("name", "Provider 名称")},
				"post": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "手动刷新指定 Provider 模型列表",
					"operationId": "refreshProviderModels",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"responses":   adminResponses(schemaRef("#/components/schemas/RefreshProviderModelsResponse")),
				},
			},
			"/admin/usage/summary": gin.H{
				"get": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "查询 token 消耗聚合统计",
					"operationId": "getUsageSummary",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"parameters": []gin.H{
						queryParam("provider", "按 Provider 过滤"),
						queryParam("model", "按模型过滤"),
						queryParam("group_by", "聚合方式：provider 或 model，默认 model"),
					},
					"responses": adminResponses(schemaRef("#/components/schemas/UsageSummaryListResponse")),
				},
			},
			"/admin/usage/records": gin.H{
				"get": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "查询 token 消耗明细记录",
					"operationId": "getUsageRecords",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"parameters": []gin.H{
						queryParam("provider", "按 Provider 过滤"),
						queryParam("model", "按模型过滤"),
						queryParam("limit", "返回记录数，默认 100，最大 1000"),
					},
					"responses": adminResponses(schemaRef("#/components/schemas/UsageRecordListResponse")),
				},
			},
			"/admin/models/check": gin.H{
				"post": gin.H{
					"tags":        []string{"Admin"},
					"summary":     "流式检测模型存活状态",
					"description": "按顺序逐个检测模型，每完成一个模型立即返回一行 NDJSON。",
					"operationId": "checkModels",
					"security":    []gin.H{{"ManagementBearerAuth": []string{}}},
					"requestBody": gin.H{"required": false, "content": jsonContent(schemaRef("#/components/schemas/AdminModelCheckRequest"))},
					"responses": gin.H{
						"200": gin.H{"description": "NDJSON 流，每行一个 ModelCheckResult", "content": gin.H{"application/x-ndjson": gin.H{"schema": schemaRef("#/components/schemas/ModelCheckResult")}}},
						"400": gin.H{"description": "请求错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"401": gin.H{"description": "管理 API Key 无效", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"403": gin.H{"description": "管理 API 未启用", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"404": gin.H{"description": "资源不存在", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
						"500": gin.H{"description": "服务内部错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
					},
				},
			},
		},
		"components": gin.H{
			"securitySchemes": gin.H{
				"ProxyBearerAuth":      gin.H{"type": "http", "scheme": "bearer", "description": "代理 API Key，对应 config.auth.keys"},
				"ManagementBearerAuth": gin.H{"type": "http", "scheme": "bearer", "description": "管理 API Key，对应 config.management.keys"},
			},
			"schemas": openAPISchemas(),
		},
	})
}

type AdminProviderRequest struct {
	Name     string   `json:"name"`
	APIType  string   `json:"api_type"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	ProxyURL string   `json:"proxy_url"`
	Models   []string `json:"models"`
	Enabled  bool     `json:"enabled"`
}

type AdminModelCheckRequest struct {
	Provider       string   `json:"provider"`
	Models         []string `json:"models"`
	Prompt         string   `json:"prompt"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type AdminModelCheckResult struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	ModelID    string `json:"model_id"`
	Alive      bool   `json:"alive"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

func (h *Handler) handleAdminListProviders(c *gin.Context) {
	providers, err := h.manager.ListAllProviders()
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取 Provider 列表失败: "+err.Error()))
		return
	}
	type safeProvider struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		APIType   string    `json:"api_type"`
		BaseURL   string    `json:"base_url"`
		APIKey    string    `json:"api_key"`
		ProxyURL  string    `json:"proxy_url"`
		Models    []string  `json:"models"`
		Enabled   bool      `json:"enabled"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	result := make([]safeProvider, 0, len(providers))
	for _, p := range providers {
		result = append(result, safeProvider{ID: p.ID, Name: p.Name, APIType: normalizeAPIType(p.APIType), BaseURL: p.BaseURL, APIKey: maskAPIKey(p.APIKey), ProxyURL: p.ProxyURL, Models: splitModels(p.Models), Enabled: p.Enabled, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": result})
}

func (h *Handler) handleAdminGetProvider(c *gin.Context) {
	name := c.Param("name")
	p, err := h.manager.GetProviderByName(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取 Provider 失败: "+err.Error()))
		return
	}
	if p == nil {
		c.JSON(http.StatusNotFound, errorResponse("Provider '"+name+"' 不存在"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": p.ID, "name": p.Name, "api_type": normalizeAPIType(p.APIType), "base_url": p.BaseURL, "api_key": maskAPIKey(p.APIKey), "proxy_url": p.ProxyURL, "models": splitModels(p.Models), "enabled": p.Enabled, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt})
}

func (h *Handler) handleAdminAddProvider(c *gin.Context) {
	var req AdminProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse("请求体解析失败: "+err.Error()))
		return
	}
	if err := validateProviderRequest(req, true); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	existing, err := h.manager.GetProviderByName(req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("检查 Provider 失败: "+err.Error()))
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, errorResponse("Provider '"+req.Name+"' 已存在"))
		return
	}
	req.APIType = normalizeAPIType(req.APIType)
	if err := h.manager.AddProvider(req.Name, req.APIType, req.BaseURL, req.APIKey, req.ProxyURL, req.Models, req.Enabled, h.FetchProviderModels); err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("新增 Provider 失败: "+err.Error()))
		return
	}
	log.Printf("[ADMIN] 新增 Provider: %s (base_url=%s, enabled=%v)", req.Name, req.BaseURL, req.Enabled)
	c.JSON(http.StatusCreated, gin.H{"message": "Provider '" + req.Name + "' 创建成功"})
}

func (h *Handler) handleAdminUpdateProvider(c *gin.Context) {
	name := c.Param("name")
	var req AdminProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse("请求体解析失败: "+err.Error()))
		return
	}
	if err := validateProviderRequest(req, false); err != nil {
		c.JSON(http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	existing, err := h.manager.GetProviderByName(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取 Provider 失败: "+err.Error()))
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, errorResponse("Provider '"+name+"' 不存在"))
		return
	}
	if shouldKeepExistingAPIKey(req.APIKey) {
		req.APIKey = existing.APIKey
	}
	if strings.TrimSpace(req.APIType) == "" {
		req.APIType = existing.APIType
	}
	req.APIType = normalizeAPIType(req.APIType)
	if err := h.manager.UpdateProvider(name, req.APIType, req.BaseURL, req.APIKey, req.ProxyURL, req.Models, req.Enabled); err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("更新 Provider 失败: "+err.Error()))
		return
	}
	log.Printf("[ADMIN] 更新 Provider: %s (base_url=%s, enabled=%v)", name, req.BaseURL, req.Enabled)
	c.JSON(http.StatusOK, gin.H{"message": "Provider '" + name + "' 更新成功"})
}

func (h *Handler) handleAdminDeleteProvider(c *gin.Context) {
	name := c.Param("name")
	if err := h.manager.DeleteProvider(name); err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("删除 Provider 失败: "+err.Error()))
		return
	}
	log.Printf("[ADMIN] 删除 Provider: %s", name)
	c.JSON(http.StatusOK, gin.H{"message": "Provider '" + name + "' 删除成功"})
}

func (h *Handler) handleAdminRefreshProviderModels(c *gin.Context) {
	name := c.Param("name")
	p, err := h.manager.GetProviderByName(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取 Provider 失败: "+err.Error()))
		return
	}
	if p == nil {
		c.JSON(http.StatusNotFound, errorResponse("Provider '"+name+"' 不存在"))
		return
	}
	if !p.Enabled {
		c.JSON(http.StatusBadRequest, errorResponse("Provider '"+name+"' 已禁用，无法刷新模型"))
		return
	}

	models, err := h.FetchProviderModels(p.APIType, p.BaseURL, p.APIKey, p.ProxyURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, errorResponse("刷新 Provider 模型失败: "+err.Error()))
		return
	}
	if len(models) == 0 {
		c.JSON(http.StatusBadGateway, errorResponse("刷新 Provider 模型失败: 上游未返回任何模型"))
		return
	}
	if err := h.manager.GetStore().UpdateProviderModels(name, models); err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("保存 Provider 模型失败: "+err.Error()))
		return
	}

	log.Printf("[ADMIN] 刷新 Provider 模型: %s (%d 个)", name, len(models))
	c.JSON(http.StatusOK, gin.H{
		"message":  "Provider '" + name + "' 模型刷新成功",
		"provider": name,
		"models":   models,
		"count":    len(models),
	})
}

func (h *Handler) handleAdminUsageSummary(c *gin.Context) {
	if h.usage == nil {
		c.JSON(http.StatusServiceUnavailable, errorResponse("token 统计插件未启用"))
		return
	}

	items, err := h.usage.Summary(tokenusage.Query{
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
		GroupBy:  c.DefaultQuery("group_by", "model"),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("查询 token 统计失败: "+err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

func (h *Handler) handleAdminUsageRecords(c *gin.Context) {
	if h.usage == nil {
		c.JSON(http.StatusServiceUnavailable, errorResponse("token 统计插件未启用"))
		return
	}

	limit := 100
	if rawLimit := c.Query("limit"); rawLimit != "" {
		fmt.Sscanf(rawLimit, "%d", &limit)
	}

	items, err := h.usage.Records(tokenusage.Query{
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
		Limit:    limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("查询 token 明细失败: "+err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

func (h *Handler) handleAdminCheckModels(c *gin.Context) {
	var req AdminModelCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, errorResponse("请求体解析失败: "+err.Error()))
		return
	}

	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 20
	}
	if timeoutSeconds > 120 {
		timeoutSeconds = 120
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "ping"
	}

	providers, err := h.providersForModelCheck(strings.TrimSpace(req.Provider))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorResponse("获取 Provider 失败: "+err.Error()))
		return
	}
	if len(providers) == 0 {
		c.JSON(http.StatusNotFound, errorResponse("没有可检测的 Provider"))
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, errorResponse("当前响应写入器不支持流式返回"))
		return
	}

	modelFilter := map[string]bool{}
	for _, model := range req.Models {
		model = strings.TrimSpace(model)
		if model != "" {
			modelFilter[model] = true
		}
	}

	c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	encoder := json.NewEncoder(c.Writer)

	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		for _, model := range splitModels(p.Models) {
			modelID := p.Name + "/" + model
			if len(modelFilter) > 0 && !modelFilter[model] && !modelFilter[modelID] {
				continue
			}
			if err := encoder.Encode(h.checkSingleModel(c.Request.Context(), p, model, prompt, timeoutSeconds)); err != nil {
				return
			}
			flusher.Flush()
			if c.Request.Context().Err() != nil {
				return
			}
		}
	}
}

func (h *Handler) providersForModelCheck(providerName string) ([]storage.Provider, error) {
	if providerName == "" {
		return h.manager.ListAllProviders()
	}
	p, err := h.manager.GetProviderByName(providerName)
	if err != nil || p == nil {
		return nil, err
	}
	return []storage.Provider{*p}, nil
}

func (h *Handler) checkSingleModel(parent context.Context, p storage.Provider, model, prompt string, timeoutSeconds int) AdminModelCheckResult {
	result := AdminModelCheckResult{Provider: p.Name, Model: model, ModelID: p.Name + "/" + model}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	start := time.Now()
	use1MContext := anthropicModelUses1MContext(model)
	resp, err := h.sendModelCheckRequest(ctx, p, model, prompt, use1MContext)
	if err == nil && normalizeAPIType(p.APIType) == "anthropic" && !use1MContext && anthropicResponseRequires1MContext(resp) {
		resp.Body.Close()
		resp, err = h.sendModelCheckRequest(ctx, p, model, prompt, true)
	}
	result.LatencyMS = time.Since(start).Milliseconds()

	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	result.Alive = resp.IsSuccessState()
	if !result.Alive {
		result.Error = truncateForResponse(resp.String(), 300)
	}
	return result
}

func (h *Handler) sendModelCheckRequest(ctx context.Context, p storage.Provider, model, prompt string, use1MContext bool) (*req.Response, error) {
	request := h.providerClient(p.ProxyURL).R().SetContext(ctx)
	if normalizeAPIType(p.APIType) == "anthropic" {
		request.SetHeader("x-api-key", p.APIKey).
			SetHeader("anthropic-version", "2023-06-01").
			SetBody(gin.H{
				"model":      model,
				"max_tokens": 1,
				"messages": []gin.H{
					{"role": "user", "content": prompt},
				},
			})
		if use1MContext {
			request.SetHeader("anthropic-beta", "context-1m-2025-08-07")
		}
		return request.Post(strings.TrimRight(p.BaseURL, "/") + "/v1/messages")
	}

	return request.SetHeader("Authorization", "Bearer "+p.APIKey).
		SetBody(gin.H{
			"model":      model,
			"stream":     false,
			"max_tokens": 1,
			"messages": []gin.H{
				{"role": "user", "content": prompt},
			},
		}).
		Post(strings.TrimRight(p.BaseURL, "/") + "/v1/chat/completions")
}

func anthropicResponseRequires1MContext(resp *req.Response) bool {
	return resp != nil && !resp.IsSuccessState() && strings.Contains(strings.ToLower(resp.String()), "1m 上下文")
}

func truncateForResponse(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "..."
}

func (h *Handler) providerClient(proxyURL string) *req.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return h.client
	}
	return h.client.Clone().SetProxyURL(proxyURL)
}

func validateProviderRequest(req AdminProviderRequest, requireName bool) error {
	if requireName && strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("name 字段不能为空")
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		return fmt.Errorf("base_url 字段不能为空")
	}
	if requireName && strings.TrimSpace(req.APIKey) == "" {
		return fmt.Errorf("api_key 字段不能为空")
	}
	apiType := normalizeAPIType(req.APIType)
	if apiType != "openai" && apiType != "anthropic" {
		return fmt.Errorf("api_type 仅支持 openai 或 anthropic")
	}
	return nil
}

func normalizeAPIType(apiType string) string {
	apiType = strings.ToLower(strings.TrimSpace(apiType))
	if apiType == "" {
		return "openai"
	}
	return apiType
}

func anthropicModelUses1MContext(model string) bool {
	return strings.Contains(strings.ToUpper(model), "[1M]")
}

func splitModels(modelsStr string) []string {
	models := []string{}
	if modelsStr == "" {
		return models
	}
	for _, model := range strings.Split(modelsStr, ",") {
		model = strings.TrimSpace(model)
		if model != "" {
			models = append(models, model)
		}
	}
	return models
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func shouldKeepExistingAPIKey(key string) bool {
	key = strings.TrimSpace(key)
	return key == "" || strings.Contains(key, "****")
}

func jsonContent(schema gin.H) gin.H {
	return gin.H{"application/json": gin.H{"schema": schema}}
}

func schemaRef(ref string) gin.H {
	return gin.H{"$ref": ref}
}

func pathParam(name, description string) gin.H {
	return gin.H{
		"name":        name,
		"in":          "path",
		"required":    true,
		"description": description,
		"schema":      gin.H{"type": "string"},
	}
}

func queryParam(name, description string) gin.H {
	return gin.H{
		"name":        name,
		"in":          "query",
		"required":    false,
		"description": description,
		"schema":      gin.H{"type": "string"},
	}
}

func genericProxyOperation(summary string) gin.H {
	return gin.H{
		"tags":        []string{"OpenAI Proxy"},
		"summary":     summary,
		"description": "通过请求体中的 model 字段解析目标 Provider，并转发到对应上游路径。",
		"security":    []gin.H{{"ProxyBearerAuth": []string{}}},
		"parameters":  []gin.H{pathParam("path", "v1 下的通用代理路径")},
		"requestBody": gin.H{"required": false, "content": jsonContent(gin.H{"type": "object", "additionalProperties": true})},
		"responses": gin.H{
			"200": gin.H{"description": "上游 Provider 返回的响应"},
			"400": gin.H{"description": "请求错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
			"401": gin.H{"description": "代理 API Key 无效", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
			"502": gin.H{"description": "上游请求失败", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		},
	}
}

func adminResponses(successSchema gin.H) gin.H {
	return gin.H{
		"200": gin.H{"description": "操作成功", "content": jsonContent(successSchema)},
		"201": gin.H{"description": "创建成功", "content": jsonContent(successSchema)},
		"400": gin.H{"description": "请求错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		"401": gin.H{"description": "管理 API Key 无效", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		"403": gin.H{"description": "管理 API 未启用", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		"404": gin.H{"description": "资源不存在", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		"409": gin.H{"description": "资源冲突", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
		"500": gin.H{"description": "服务内部错误", "content": jsonContent(schemaRef("#/components/schemas/ErrorResponse"))},
	}
}

func openAPISchemas() gin.H {
	return gin.H{
		"HealthResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"status":       gin.H{"type": "string", "example": "ok"},
				"providers":    gin.H{"type": "integer", "example": 4},
				"total_models": gin.H{"type": "integer", "example": 20},
			},
		},
		"ModelEntry": gin.H{
			"type": "object",
			"properties": gin.H{
				"id":       gin.H{"type": "string", "example": "openai/gpt-4o"},
				"object":   gin.H{"type": "string", "example": "model"},
				"created":  gin.H{"type": "integer", "format": "int64"},
				"owned_by": gin.H{"type": "string", "example": "openai"},
			},
		},
		"ModelListResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"object": gin.H{"type": "string", "example": "list"},
				"data":   gin.H{"type": "array", "items": schemaRef("#/components/schemas/ModelEntry")},
			},
		},
		"ChatCompletionRequest": gin.H{
			"type":     "object",
			"required": []string{"model", "messages"},
			"properties": gin.H{
				"model":    gin.H{"type": "string", "description": "支持 Provider/model 或直接模型名", "example": "openai/gpt-4o"},
				"stream":   gin.H{"type": "boolean", "example": false},
				"messages": gin.H{"type": "array", "items": gin.H{"type": "object", "additionalProperties": true}},
			},
			"additionalProperties": true,
		},
		"AdminProviderRequest": gin.H{
			"type":     "object",
			"required": []string{"base_url", "api_key"},
			"properties": gin.H{
				"name":      gin.H{"type": "string", "description": "创建 Provider 时必填，更新时由路径指定", "example": "openai"},
				"api_type":  gin.H{"type": "string", "enum": []string{"openai", "anthropic"}, "default": "openai"},
				"base_url":  gin.H{"type": "string", "example": "https://api.example.com"},
				"api_key":   gin.H{"type": "string", "example": "sk-xxxxxxxx"},
				"proxy_url": gin.H{"type": "string", "description": "可选，上游请求代理地址", "example": "http://127.0.0.1:7890"},
				"models":    gin.H{"type": "array", "items": gin.H{"type": "string"}, "example": []string{"gpt-4o", "gpt-4o-mini"}},
				"enabled":   gin.H{"type": "boolean", "example": true},
			},
		},
		"SafeProvider": gin.H{
			"type": "object",
			"properties": gin.H{
				"id":         gin.H{"type": "integer", "format": "int64"},
				"name":       gin.H{"type": "string"},
				"api_type":   gin.H{"type": "string", "enum": []string{"openai", "anthropic"}},
				"base_url":   gin.H{"type": "string"},
				"api_key":    gin.H{"type": "string", "description": "脱敏后的 API Key"},
				"proxy_url":  gin.H{"type": "string", "description": "上游请求代理地址，空值表示直连"},
				"models":     gin.H{"type": "array", "items": gin.H{"type": "string"}},
				"enabled":    gin.H{"type": "boolean"},
				"created_at": gin.H{"type": "string", "format": "date-time"},
				"updated_at": gin.H{"type": "string", "format": "date-time"},
			},
		},
		"ProviderListResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"object": gin.H{"type": "string", "example": "list"},
				"data":   gin.H{"type": "array", "items": schemaRef("#/components/schemas/SafeProvider")},
			},
		},
		"MessageResponse": gin.H{
			"type":       "object",
			"properties": gin.H{"message": gin.H{"type": "string"}},
		},
		"RefreshProviderModelsResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"message":  gin.H{"type": "string"},
				"provider": gin.H{"type": "string"},
				"models":   gin.H{"type": "array", "items": gin.H{"type": "string"}},
				"count":    gin.H{"type": "integer"},
			},
		},
		"AdminModelCheckRequest": gin.H{
			"type": "object",
			"properties": gin.H{
				"provider":        gin.H{"type": "string", "description": "可选，仅检测指定 Provider", "example": "openai"},
				"models":          gin.H{"type": "array", "items": gin.H{"type": "string"}, "description": "可选，支持模型名或 Provider/model", "example": []string{"openai/gpt-4o"}},
				"prompt":          gin.H{"type": "string", "description": "可选，探活提示词", "example": "ping"},
				"timeout_seconds": gin.H{"type": "integer", "description": "单模型超时时间，默认 20，最大 120", "example": 20},
			},
		},
		"ModelCheckResult": gin.H{
			"type": "object",
			"properties": gin.H{
				"provider":    gin.H{"type": "string"},
				"model":       gin.H{"type": "string"},
				"model_id":    gin.H{"type": "string", "example": "openai/gpt-4o"},
				"alive":       gin.H{"type": "boolean"},
				"status_code": gin.H{"type": "integer"},
				"latency_ms":  gin.H{"type": "integer", "format": "int64"},
				"error":       gin.H{"type": "string"},
			},
		},
		"UsageSummary": gin.H{
			"type": "object",
			"properties": gin.H{
				"provider":          gin.H{"type": "string"},
				"model":             gin.H{"type": "string"},
				"request_count":     gin.H{"type": "integer", "format": "int64"},
				"prompt_tokens":     gin.H{"type": "integer", "format": "int64"},
				"completion_tokens": gin.H{"type": "integer", "format": "int64"},
				"total_tokens":      gin.H{"type": "integer", "format": "int64"},
			},
		},
		"UsageSummaryListResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"object": gin.H{"type": "string", "example": "list"},
				"data":   gin.H{"type": "array", "items": schemaRef("#/components/schemas/UsageSummary")},
			},
		},
		"UsageRecord": gin.H{
			"type": "object",
			"properties": gin.H{
				"id":                gin.H{"type": "integer", "format": "int64"},
				"provider":          gin.H{"type": "string"},
				"model":             gin.H{"type": "string"},
				"endpoint":          gin.H{"type": "string"},
				"prompt_tokens":     gin.H{"type": "integer"},
				"completion_tokens": gin.H{"type": "integer"},
				"total_tokens":      gin.H{"type": "integer"},
				"created_at":        gin.H{"type": "string", "format": "date-time"},
			},
		},
		"UsageRecordListResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"object": gin.H{"type": "string", "example": "list"},
				"data":   gin.H{"type": "array", "items": schemaRef("#/components/schemas/UsageRecord")},
			},
		},
		"ErrorResponse": gin.H{
			"type": "object",
			"properties": gin.H{
				"error": gin.H{"type": "object", "properties": gin.H{"message": gin.H{"type": "string"}, "type": gin.H{"type": "string"}}},
			},
		},
	}
}

func errorResponse(message string) gin.H { return gin.H{"error": gin.H{"message": message}} }
func copyUpstreamHeaders(w http.ResponseWriter, header http.Header) {
	for key, values := range header {
		if shouldSkipResponseHeader(key) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
}

func shouldSkipResponseHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "content-length", "content-encoding":
		return true
	default:
		return false
	}
}

func ToStorageModelEntries(providerName string, models []string) []storage.ModelEntry {
	entries := make([]storage.ModelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, storage.ModelEntry{ID: fmt.Sprintf("%s/%s", providerName, m), Provider: providerName, Object: "model", OwnedBy: providerName})
	}
	return entries
}

type modelsAPIResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (h *Handler) FetchProviderModels(apiType, baseURL, apiKey, proxyURL string) ([]string, error) {
	targetURL := strings.TrimRight(baseURL, "/") + "/v1/models"
	request := h.providerClient(proxyURL).R()
	if normalizeAPIType(apiType) == "anthropic" {
		request.SetHeader("x-api-key", apiKey).SetHeader("anthropic-version", "2023-06-01")
	} else {
		request.SetHeader("Authorization", "Bearer "+apiKey)
	}
	resp, err := request.Get(targetURL)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", targetURL, err)
	}
	defer resp.Body.Close()
	if !resp.IsSuccessState() {
		return nil, fmt.Errorf("%s 返回状态码 %d: %s", targetURL, resp.StatusCode, resp.String())
	}
	var result modelsAPIResponse
	if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("解析 %s 响应失败: %w", targetURL, err)
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func (h *Handler) GetClient() *req.Client { return h.client }
