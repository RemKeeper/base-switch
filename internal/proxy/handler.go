package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"baseSwitch/internal/config"
	"baseSwitch/internal/provider"
	"baseSwitch/internal/storage"

	"github.com/imroc/req/v3"
)

// Handler 反向代理处理器
type Handler struct {
	manager  *provider.Manager
	client   *req.Client
	auth     *config.AuthConfig      // 前置鉴权配置
	mgmtAuth *config.ManagementConfig // 管理 API 鉴权配置
}

// NewHandler 创建代理处理器，启用 Chrome 指纹伪装
func NewHandler(mgr *provider.Manager, auth *config.AuthConfig, mgmtAuth *config.ManagementConfig) *Handler {
	client := req.C().
		ImpersonateChrome().                                 // Chrome 指纹伪装
		SetTimeout(5*time.Minute).                           // AI 请求可能较长
		SetCommonHeader("Content-Type", "application/json"). // 通用 JSON Content-Type
		SetCommonHeader("Accept", "application/json, text/event-stream, */*")

	// 底层连接池复用
	client.GetClient().Transport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Handler{
		manager:  mgr,
		client:   client,
		auth:     auth,
		mgmtAuth: mgmtAuth,
	}
}

// ChatRequest OpenAI 兼容的请求体
type ChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream,omitempty"`
	Messages []any  `json:"messages,omitempty"`
}

// ModelEntry 模型列表条目
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelListResponse 模型列表响应
type ModelListResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// HandleModels 处理 /v1/models 请求
func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	entries, err := h.manager.GetAllModels()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"获取模型列表失败: %s"}}`, err.Error()),
			http.StatusInternalServerError)
		return
	}

	data := make([]ModelEntry, 0, len(entries))
	for _, e := range entries {
		data = append(data, ModelEntry{
			ID:      e.ID,
			Object:  e.Object,
			Created: e.Created,
			OwnedBy: e.OwnedBy,
		})
	}

	resp := ModelListResponse{
		Object: "list",
		Data:   data,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleChatCompletions 处理 /v1/chat/completions 请求
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"读取请求体失败"}}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var chatReq ChatRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		http.Error(w, `{"error":{"message":"解析请求体失败"}}`, http.StatusBadRequest)
		return
	}

	if chatReq.Model == "" {
		http.Error(w, `{"error":{"message":"model 字段不能为空"}}`, http.StatusBadRequest)
		return
	}

	prov, actualModel, err := h.manager.ResolveModel(chatReq.Model)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()),
			http.StatusBadRequest)
		return
	}

	log.Printf("[PROXY] %s -> %s/%s (stream=%v)", chatReq.Model, prov.Name, actualModel, chatReq.Stream)

	newBody := h.rewriteModelField(bodyBytes, chatReq.Model, actualModel)
	targetURL := strings.TrimRight(prov.BaseURL, "/") + "/v1/chat/completions"

	if chatReq.Stream {
		h.forwardStream(w, prov.APIKey, targetURL, newBody)
		return
	}

	h.forwardNonStream(w, prov.APIKey, targetURL, newBody)
}

// forwardNonStream 使用 req/v3 转发非流式请求
func (h *Handler) forwardNonStream(w http.ResponseWriter, apiKey, targetURL string, body []byte) {
	resp, err := h.client.R().
		SetHeader("Authorization", "Bearer "+apiKey).
		SetBodyJsonBytes(body).
		Post(targetURL)

	if err != nil {
		log.Printf("[ERROR] 转发请求失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"上游请求失败: %s"}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Bytes())
}

// forwardStream 使用 req/v3 转发 SSE 流式请求
func (h *Handler) forwardStream(w http.ResponseWriter, apiKey, targetURL string, body []byte) {
	resp, err := h.client.R().
		SetHeader("Authorization", "Bearer "+apiKey).
		SetBodyJsonBytes(body).
		DisableAutoReadResponse(). // 不自动读取 body，手动处理流
		Post(targetURL)

	if err != nil {
		log.Printf("[ERROR] 转发流式请求失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"上游请求失败: %s"}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("[ERROR] 不支持流式响应")
		return
	}

	// 从底层 *http.Response.Body 直接流式读取
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Response.Body.Read(buf)
		if n > 0 {
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

// rewriteModelField 重写请求体中的 model 字段
func (h *Handler) rewriteModelField(body []byte, originalModel, actualModel string) []byte {
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

// ServeHTTP 实现 http.Handler 接口，提供统一路由
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 健康检查跳过鉴权
	if r.URL.Path == "/health" {
		h.handleHealth(w, r)
		return
	}

	// 管理 API 路由（使用独立的管理密钥鉴权）
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		h.handleAdmin(w, r)
		return
	}

	// 前置 API Key 鉴权
	if !h.authenticate(w, r) {
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/") {
		h.handleV1(w, r)
		return
	}

	http.NotFound(w, r)
}

// authenticate 校验 Bearer Token
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if h.auth == nil || !h.auth.IsAuthEnabled() {
		return true // 未启用鉴权，放行
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "缺少 Authorization header，请提供 Bearer API Key",
				"type":    "authentication_error",
			},
		})
		return false
	}

	// 支持 "Bearer <key>" 格式
	key := strings.TrimPrefix(authHeader, "Bearer ")

	if key == authHeader || key == "" {
		// 没有 Bearer 前缀，尝试直接使用整个 header 值作为 key
		key = authHeader
	}

	if !h.auth.IsValidKey(key) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "无效的 API Key，拒绝访问",
				"type":    "authentication_error",
			},
		})
		return false
	}

	return true
}

// handleV1 处理 /v1/ 路径
func (h *Handler) handleV1(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/models" && r.Method == "GET":
		h.HandleModels(w, r)
	case r.URL.Path == "/v1/chat/completions" && r.Method == "POST":
		h.HandleChatCompletions(w, r)
	default:
		if r.Method == "POST" || r.Method == "GET" {
			h.handleGenericProxy(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

// handleGenericProxy 通用代理转发（使用 req/v3）
func (h *Handler) handleGenericProxy(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"读取请求体失败"}}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var raw map[string]any
	modelStr := ""
	if json.Unmarshal(bodyBytes, &raw) == nil {
		if m, ok := raw["model"].(string); ok {
			modelStr = m
		}
	}

	if modelStr == "" {
		http.Error(w, `{"error":{"message":"无法确定目标 provider，请使用 Provider/model 格式指定模型"}}`, http.StatusBadRequest)
		return
	}

	prov, actualModel, err := h.manager.ResolveModel(modelStr)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), http.StatusBadRequest)
		return
	}

	newBody := h.rewriteModelField(bodyBytes, modelStr, actualModel)
	targetURL := strings.TrimRight(prov.BaseURL, "/") + r.URL.Path

	log.Printf("[PROXY] %s %s -> %s/%s", r.Method, r.URL.Path, prov.Name, actualModel)

	resp, err := h.client.R().
		SetHeader("Authorization", "Bearer "+prov.APIKey).
		SetBodyJsonBytes(newBody).
		Post(targetURL)

	if err != nil {
		log.Printf("[ERROR] 转发请求失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"上游请求失败: %s"}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Bytes())
}

// handleHealth 健康检查
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	count, err := h.manager.GetProviderCount()
	if err != nil {
		count = -1
	}

	entries, _ := h.manager.GetAllModels()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":       "ok",
		"providers":    count,
		"total_models": len(entries),
	})
}

// ──────────────── 管理 API ────────────────

// handleAdmin 管理 API 路由分发
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	// 管理 API 鉴权
	if !h.authenticateManagement(w, r) {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin")

	switch {
	case path == "/providers" && r.Method == "GET":
		h.handleAdminListProviders(w, r)
	case path == "/providers" && r.Method == "POST":
		h.handleAdminAddProvider(w, r)
	case strings.HasPrefix(path, "/providers/") && r.Method == "GET":
		name := strings.TrimPrefix(path, "/providers/")
		h.handleAdminGetProvider(w, r, name)
	case strings.HasPrefix(path, "/providers/") && r.Method == "PUT":
		name := strings.TrimPrefix(path, "/providers/")
		h.handleAdminUpdateProvider(w, r, name)
	case strings.HasPrefix(path, "/providers/") && r.Method == "DELETE":
		name := strings.TrimPrefix(path, "/providers/")
		h.handleAdminDeleteProvider(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// authenticateManagement 管理 API Key 鉴权
func (h *Handler) authenticateManagement(w http.ResponseWriter, r *http.Request) bool {
	if h.mgmtAuth == nil || !h.mgmtAuth.IsManagementEnabled() {
		// 未启用管理鉴权时拒绝所有管理请求（安全起见）
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "管理 API 未启用",
				"type":    "admin_disabled",
			},
		})
		return false
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "缺少 Authorization header，请提供管理 API Key",
				"type":    "authentication_error",
			},
		})
		return false
	}

	key := strings.TrimPrefix(authHeader, "Bearer ")
	if key == authHeader || key == "" {
		key = authHeader
	}

	if !h.mgmtAuth.IsValidManagementKey(key) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "无效的管理 API Key，拒绝访问",
				"type":    "authentication_error",
			},
		})
		return false
	}

	return true
}

// AdminProviderRequest 管理 API 的 Provider 请求体
type AdminProviderRequest struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
	Enabled bool     `json:"enabled"`
}

// handleAdminListProviders GET /admin/providers - 列出所有 Provider
func (h *Handler) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.manager.ListAllProviders()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "获取 Provider 列表失败: "+err.Error())
		return
	}

	// 返回时隐藏 api_key（仅显示前后几位）
	type safeProvider struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		BaseURL   string    `json:"base_url"`
		APIKey    string    `json:"api_key"`
		Models    []string  `json:"models"`
		Enabled   bool      `json:"enabled"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	result := make([]safeProvider, 0, len(providers))
	for _, p := range providers {
		models := []string{}
		if p.Models != "" {
			models = strings.Split(p.Models, ",")
		}
		maskedKey := maskAPIKey(p.APIKey)
		result = append(result, safeProvider{
			ID:        p.ID,
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			APIKey:    maskedKey,
			Models:    models,
			Enabled:   p.Enabled,
			CreatedAt: p.CreatedAt,
			UpdatedAt: p.UpdatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   result,
	})
}

// handleAdminGetProvider GET /admin/providers/:name - 获取单个 Provider
func (h *Handler) handleAdminGetProvider(w http.ResponseWriter, r *http.Request, name string) {
	p, err := h.manager.GetProviderByName(name)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "获取 Provider 失败: "+err.Error())
		return
	}
	if p == nil {
		writeAdminError(w, http.StatusNotFound, "Provider '"+name+"' 不存在")
		return
	}

	models := []string{}
	if p.Models != "" {
		models = strings.Split(p.Models, ",")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":         p.ID,
		"name":       p.Name,
		"base_url":   p.BaseURL,
		"api_key":    maskAPIKey(p.APIKey),
		"models":     models,
		"enabled":    p.Enabled,
		"created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt,
	})
}

// handleAdminAddProvider POST /admin/providers - 新增 Provider
func (h *Handler) handleAdminAddProvider(w http.ResponseWriter, r *http.Request) {
	var req AdminProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	if req.Name == "" {
		writeAdminError(w, http.StatusBadRequest, "name 字段不能为空")
		return
	}
	if req.BaseURL == "" {
		writeAdminError(w, http.StatusBadRequest, "base_url 字段不能为空")
		return
	}
	if req.APIKey == "" {
		writeAdminError(w, http.StatusBadRequest, "api_key 字段不能为空")
		return
	}

	// 检查是否已存在
	existing, err := h.manager.GetProviderByName(req.Name)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "检查 Provider 失败: "+err.Error())
		return
	}
	if existing != nil {
		writeAdminError(w, http.StatusConflict, "Provider '"+req.Name+"' 已存在")
		return
	}

	if err := h.manager.AddProvider(req.Name, req.BaseURL, req.APIKey, req.Models, req.Enabled); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "新增 Provider 失败: "+err.Error())
		return
	}

	log.Printf("[ADMIN] 新增 Provider: %s (base_url=%s, enabled=%v)", req.Name, req.BaseURL, req.Enabled)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"message": "Provider '" + req.Name + "' 创建成功",
	})
}

// handleAdminUpdateProvider PUT /admin/providers/:name - 更新 Provider
func (h *Handler) handleAdminUpdateProvider(w http.ResponseWriter, r *http.Request, name string) {
	var req AdminProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	if req.BaseURL == "" {
		writeAdminError(w, http.StatusBadRequest, "base_url 字段不能为空")
		return
	}
	if req.APIKey == "" {
		writeAdminError(w, http.StatusBadRequest, "api_key 字段不能为空")
		return
	}

	if err := h.manager.UpdateProvider(name, req.BaseURL, req.APIKey, req.Models, req.Enabled); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "更新 Provider 失败: "+err.Error())
		return
	}

	log.Printf("[ADMIN] 更新 Provider: %s (base_url=%s, enabled=%v)", name, req.BaseURL, req.Enabled)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"message": "Provider '" + name + "' 更新成功",
	})
}

// handleAdminDeleteProvider DELETE /admin/providers/:name - 删除 Provider
func (h *Handler) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.manager.DeleteProvider(name); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "删除 Provider 失败: "+err.Error())
		return
	}

	log.Printf("[ADMIN] 删除 Provider: %s", name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"message": "Provider '" + name + "' 删除成功",
	})
}

// maskAPIKey 对 API Key 进行脱敏显示
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// writeAdminError 写管理 API 错误响应
func writeAdminError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
		},
	})
}

// ToStorageModelEntries 将 provider 模型转为存储条目
func ToStorageModelEntries(providerName string, models []string) []storage.ModelEntry {
	entries := make([]storage.ModelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, storage.ModelEntry{
			ID:       fmt.Sprintf("%s/%s", providerName, m),
			Provider: providerName,
			Object:   "model",
			OwnedBy:  providerName,
		})
	}
	return entries
}

// modelsAPIResponse OpenAI /v1/models 响应格式
type modelsAPIResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchProviderModels 调用 Provider 的 /v1/models 端点获取模型列表
func (h *Handler) FetchProviderModels(baseURL, apiKey string) ([]string, error) {
	targetURL := strings.TrimRight(baseURL, "/") + "/v1/models"

	resp, err := h.client.R().
		SetHeader("Authorization", "Bearer "+apiKey).
		Get(targetURL)

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

// GetClient 暴露 req.Client（供外部使用）
func (h *Handler) GetClient() *req.Client {
	return h.client
}
