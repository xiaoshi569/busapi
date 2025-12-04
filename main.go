package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
)

// ==================== 配置结构 ====================

type PoolConfig struct {
	TargetCount            int  `json:"target_count"`              // 目标账号数量
	MinCount               int  `json:"min_count"`                 // 最小账号数，低于此值触发注册
	CheckIntervalMinutes   int  `json:"check_interval_minutes"`    // 检查间隔(分钟)
	RegisterThreads        int  `json:"register_threads"`          // 注册线程数
	RegisterHeadless       bool `json:"register_headless"`         // 无头模式
	RefreshOnStartup       bool `json:"refresh_on_startup"`        // 启动时刷新账号
	RefreshCooldownSec     int  `json:"refresh_cooldown_sec"`      // 刷新冷却时间(秒)
	UseCooldownSec         int  `json:"use_cooldown_sec"`          // 使用冷却时间(秒)
	MaxFailCount           int  `json:"max_fail_count"`            // 最大连续失败次数
	EnableBrowserRefresh   bool `json:"enable_browser_refresh"`    // 启用浏览器刷新401账号
	BrowserRefreshHeadless bool `json:"browser_refresh_headless"`  // 浏览器刷新无头模式
	BrowserRefreshMaxRetry int  `json:"browser_refresh_max_retry"` // 浏览器刷新最大重试次数(0=禁用)
}

type AppConfig struct {
	APIKeys       []string   `json:"api_keys"`       // API 密钥列表
	ListenAddr    string     `json:"listen_addr"`    // 监听地址
	DataDir       string     `json:"data_dir"`       // 数据目录
	Pool          PoolConfig `json:"pool"`           // 号池配置
	Proxy         string     `json:"proxy"`          // 代理
	DefaultConfig string     `json:"default_config"` // 默认 configId
}

var appConfig = AppConfig{
	ListenAddr: ":8000",
	DataDir:    "./data",
	Pool: PoolConfig{
		TargetCount:            50,
		MinCount:               10,
		CheckIntervalMinutes:   30,
		RegisterThreads:        1,
		RegisterHeadless:       true,
		RefreshOnStartup:       true,
		RefreshCooldownSec:     240, // 4分钟
		UseCooldownSec:         15,  // 15秒
		MaxFailCount:           3,
		EnableBrowserRefresh:   true, // 默认启用浏览器刷新
		BrowserRefreshHeadless: true,
		BrowserRefreshMaxRetry: 1, // 浏览器刷新最多重试1次
	},
}

// 兼容旧的环境变量
var (
	DataDir       string
	Proxy         string
	ListenAddr    string
	DefaultConfig string
	JwtTTL        = 270 * time.Second
)

// 保存默认配置到文件
func saveDefaultConfig(configPath string) error {
	data, err := json.MarshalIndent(appConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func loadAppConfig() {
	// 尝试加载配置文件
	configPath := "config.json"
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &appConfig); err != nil {
			log.Printf("⚠️ 解析配置文件失败: %v，使用默认配置", err)
		} else {
			log.Printf("✅ 加载配置文件: %s", configPath)
		}
	} else if os.IsNotExist(err) {
		// 配置文件不存在，创建默认配置
		log.Printf("⚠️ 配置文件不存在，创建默认配置: %s", configPath)
		if err := saveDefaultConfig(configPath); err != nil {
			log.Printf("❌ 创建默认配置失败: %v", err)
		}
	}

	// 环境变量覆盖配置文件
	if v := os.Getenv("DATA_DIR"); v != "" {
		appConfig.DataDir = v
	}
	if v := os.Getenv("PROXY"); v != "" {
		appConfig.Proxy = v
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		appConfig.ListenAddr = v
	}
	if v := os.Getenv("CONFIG_ID"); v != "" {
		appConfig.DefaultConfig = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		appConfig.APIKeys = append(appConfig.APIKeys, v)
	}

	// 设置全局变量
	DataDir = appConfig.DataDir
	Proxy = appConfig.Proxy
	ListenAddr = appConfig.ListenAddr
	DefaultConfig = appConfig.DefaultConfig

	// 应用号池配置
	SetCooldowns(appConfig.Pool.RefreshCooldownSec, appConfig.Pool.UseCooldownSec)
	if appConfig.Pool.MaxFailCount > 0 {
		MaxFailCount = appConfig.Pool.MaxFailCount
	}
	EnableBrowserRefresh = appConfig.Pool.EnableBrowserRefresh
	BrowserRefreshHeadless = appConfig.Pool.BrowserRefreshHeadless
	if appConfig.Pool.BrowserRefreshMaxRetry >= 0 {
		BrowserRefreshMaxRetry = appConfig.Pool.BrowserRefreshMaxRetry
	}

	if EnableBrowserRefresh && BrowserRefreshMaxRetry > 0 {
		log.Printf("🌐 浏览器刷新已启用 (headless=%v, 最大重试=%d)", BrowserRefreshHeadless, BrowserRefreshMaxRetry)
	} else if EnableBrowserRefresh {
		log.Printf("🌐 浏览器刷新已禁用 (max_retry=0)")
		EnableBrowserRefresh = false
	}
}

var FixedModels = []string{
	"gemini-2.5-flash",
	"gemini-2.5-pro",
	"gemini-3-pro-preview",
	"gemini-3-pro",
	"gemini-2.5-flash-image",
	"gemini-2.5-pro-image",
	"gemini-3-pro-preview-image",
	"gemini-3-pro-image",
	"gemini-2.5-flash-video",
	"gemini-2.5-pro-video",
	"gemini-3-pro-preview-video",
	"gemini-3-pro-video",
	"gemini-2.5-flash-search",
	"gemini-2.5-pro-search",
	"gemini-3-pro-preview-search",
	"gemini-3-pro-search",
}

// 模型名称映射到 Google API 的 modelId
var modelMapping = map[string]string{
	"gemini-2.5-flash":     "gemini-2.5-flash",
	"gemini-2.5-pro":       "gemini-2.5-pro",
	"gemini-3-pro-preview": "gemini-3-pro-preview",
	"gemini-3-pro":         "gemini-3-pro",
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// 数据结构和号池管理已移至 pool.go
// HTTP客户端和工具函数已移至 utils.go

// ==================== Session 管理 ====================

func getCommonHeaders(jwt, origAuth string) map[string]string {
	headers := map[string]string{
		"accept":             "*/*",
		"accept-encoding":    "gzip, deflate, br, zstd",
		"accept-language":    "zh-CN,zh;q=0.9,en;q=0.8",
		"authorization":      "Bearer " + jwt,
		"content-type":       "application/json",
		"origin":             "https://business.gemini.google",
		"referer":            "https://business.gemini.google/",
		"user-agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		"x-server-timeout":   "1800",
		"sec-ch-ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "cross-site",
	}
	// 同时携带原始 authorization
	if origAuth != "" {
		headers["x-original-authorization"] = origAuth
	}
	return headers
}

func createSession(jwt, configID, origAuth string) (string, error) {
	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"createSessionRequest": map[string]interface{}{
			"session": map[string]string{"name": "", "displayName": ""},
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetCreateSession", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("createSession 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("createSession 失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Session struct {
			Name string `json:"name"`
		} `json:"session"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析 session 响应失败: %w", err)
	}

	return result.Session.Name, nil
}

// 上传图片到 Session，返回 fileId（支持 base64 或 URL）
func uploadContextFile(jwt, configID, sessionName, mimeType, base64Content, origAuth string) (string, error) {
	ext := "jpg"
	if parts := strings.Split(mimeType, "/"); len(parts) == 2 {
		ext = parts[1]
	}
	fileName := fmt.Sprintf("upload_%d_%s.%s", time.Now().Unix(), uuid.New().String()[:6], ext)

	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"addContextFileRequest": map[string]interface{}{
			"name":         sessionName,
			"fileName":     fileName,
			"mimeType":     mimeType,
			"fileContents": base64Content,
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetAddContextFile", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传文件请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("上传文件失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AddContextFileResponse struct {
			FileID string `json:"fileId"`
		} `json:"addContextFileResponse"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析上传响应失败: %w", err)
	}

	if result.AddContextFileResponse.FileID == "" {
		return "", fmt.Errorf("上传成功但 fileId 为空，响应: %s", string(respBody))
	}

	return result.AddContextFileResponse.FileID, nil
}

// 通过 URL 上传图片到 Session，返回 fileId
func uploadContextFileByURL(jwt, configID, sessionName, imageURL, origAuth string) (string, error) {
	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"addContextFileRequest": map[string]interface{}{
			"name":    sessionName,
			"fileUri": imageURL,
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetAddContextFile", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传文件请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("URL上传文件失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AddContextFileResponse struct {
			FileID string `json:"fileId"`
		} `json:"addContextFileResponse"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析上传响应失败: %w", err)
	}

	if result.AddContextFileResponse.FileID == "" {
		return "", fmt.Errorf("URL上传成功但 fileId 为空，响应: %s", string(respBody))
	}

	return result.AddContextFileResponse.FileID, nil
}

type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`                // string 或 []ContentPart
	Name       string      `json:"name,omitempty"`         // 函数名称（tool角色时）
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`   // 工具调用（assistant角色时）
	ToolCallID string      `json:"tool_call_id,omitempty"` // 工具调用ID（tool角色时）
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

// OpenAI格式的工具定义
type ToolDef struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// 工具调用结果
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float64   `json:"temperature"`
	TopP        float64   `json:"top_p"`
	Tools       []ToolDef `json:"tools,omitempty"`       // 工具定义
	ToolChoice  string    `json:"tool_choice,omitempty"` // "auto", "none", "required"
}

type ChatChoice struct {
	Index        int                    `json:"index"`
	Delta        map[string]interface{} `json:"delta,omitempty"`
	Message      map[string]interface{} `json:"message,omitempty"`
	FinishReason *string                `json:"finish_reason"`
}

type ChatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
}

func createChunk(id string, created int64, model string, delta map[string]interface{}, finishReason *string) string {
	chunk := ChatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
	data, _ := json.Marshal(chunk)
	return string(data)
}

func extractContentFromReply(replyMap map[string]interface{}, jwt, session, configID, origAuth string) (text string, imageData string, imageMime string, reasoning string) {
	groundedContent, ok := replyMap["groundedContent"].(map[string]interface{})
	if !ok {
		return
	}
	content, ok := groundedContent["content"].(map[string]interface{})
	if !ok {
		return
	}

	// 检查是否是思考内容
	if thought, ok := content["thought"].(bool); ok && thought {
		if t, ok := content["text"].(string); ok && t != "" {
			reasoning = t
		}
		return
	}

	// 提取文本
	if t, ok := content["text"].(string); ok && t != "" {
		text = t
	}

	// 提取图片 (inlineData - 直接返回 base64)
	if inlineData, ok := content["inlineData"].(map[string]interface{}); ok {
		if mime, ok := inlineData["mimeType"].(string); ok {
			imageMime = mime
		}
		if data, ok := inlineData["data"].(string); ok {
			imageData = data
		}
	}

	// 提取文件 (file - 需要下载，可能是图片或视频)
	if file, ok := content["file"].(map[string]interface{}); ok {
		fileId, _ := file["fileId"].(string)
		mimeType, _ := file["mimeType"].(string)
		if fileId != "" {
			// 根据 mimeType 判断类型
			fileType := "文件"
			if strings.HasPrefix(mimeType, "image/") {
				fileType = "图片"
			} else if strings.HasPrefix(mimeType, "video/") {
				fileType = "视频"
			}
			//	log.Printf("📥 发现%s: fileId=%s, mimeType=%s", fileType, fileId, mimeType)
			data, err := downloadGeneratedFile(jwt, fileId, session, configID, origAuth)
			if err != nil {
				log.Printf("❌ 下载%s失败: %v", fileType, err)
			} else {
				imageData = data
				imageMime = mimeType
			}
		}
	}

	return
}

// 下载生成的文件（图片或视频）——带重试机制
func downloadGeneratedFile(jwt, fileId, session, configID, origAuth string) (string, error) {
	return downloadGeneratedFileWithRetry(jwt, fileId, session, configID, origAuth, 3)
}

// downloadGeneratedFileWithRetry 下载文件，带重试机制，遇到 401 时尝试切换账号
func downloadGeneratedFileWithRetry(jwt, fileId, session, configID, origAuth string, maxRetries int) (string, error) {
	// 参数验证
	if jwt == "" {
		return "", fmt.Errorf("JWT 为空，无法下载文件")
	}
	if session == "" {
		return "", fmt.Errorf("session 为空，无法下载文件")
	}
	if configID == "" {
		return "", fmt.Errorf("configID 为空，无法下载文件")
	}

	log.Printf("📥 开始下载文件: fileId=%s, session=%s", fileId, session)

	var lastErr error
	currentJWT := jwt
	currentOrigAuth := origAuth

	for retry := 0; retry < maxRetries; retry++ {
		result, err := downloadGeneratedFileOnce(currentJWT, fileId, session, configID, currentOrigAuth)
		if err == nil {
			return result, nil
		}

		lastErr = err
		errMsg := err.Error()

		// 检查是否是 401/403 错误
		if strings.Contains(errMsg, "401") || strings.Contains(errMsg, "403") ||
			strings.Contains(errMsg, "UNAUTHENTICATED") || strings.Contains(errMsg, "SESSION_COOKIE_INVALID") {
			log.Printf("⚠️ 下载文件认证失败 (尝试 %d/%d): %v，尝试切换账号...", retry+1, maxRetries, err)

			// 尝试获取新账号
			newAcc := pool.Next()
			if newAcc != nil {
				newJWT, newConfigID, jwtErr := newAcc.GetJWT()
				if jwtErr == nil {
					log.Printf("✅ 切换到新账号: %s", newAcc.Data.Email)
					currentJWT = newJWT
					currentOrigAuth = newAcc.Data.Authorization
					// 如果新账号有不同的 configID，也可以更新（但通常 session 是绑定的）
					_ = newConfigID
					continue
				}
			}
			log.Printf("❌ 无法获取新账号，重试当前账号...")
		}

		// 其他错误，等待后重试
		log.Printf("❌ 下载文件失败 (尝试 %d/%d): %v", retry+1, maxRetries, err)
		time.Sleep(500 * time.Millisecond)
	}

	return "", fmt.Errorf("下载文件失败，已重试 %d 次: %w", maxRetries, lastErr)
}

// downloadGeneratedFileOnce 单次下载文件尝试
func downloadGeneratedFileOnce(jwt, fileId, session, configID, origAuth string) (string, error) {

	// 步骤1: 使用 widgetListSessionFileMetadata 获取文件下载 URL
	listBody := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"listSessionFileMetadataRequest": map[string]interface{}{
			"name":   session,
			"filter": "file_origin_type = AI_GENERATED",
		},
	}
	listBodyBytes, _ := json.Marshal(listBody)

	listReq, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetListSessionFileMetadata", bytes.NewReader(listBodyBytes))
	for k, v := range getCommonHeaders(jwt, origAuth) {
		listReq.Header.Set(k, v)
	}

	listResp, err := httpClient.Do(listReq)
	if err != nil {
		return "", fmt.Errorf("获取文件元数据失败: %w", err)
	}
	defer listResp.Body.Close()

	listRespBody, _ := readResponseBody(listResp)

	if listResp.StatusCode != 200 {
		return "", fmt.Errorf("获取文件元数据失败: HTTP %d: %s", listResp.StatusCode, string(listRespBody))
	}

	// 解析响应，查找匹配的 fileId
	var listResult struct {
		ListSessionFileMetadataResponse struct {
			FileMetadata []struct {
				FileID      string `json:"fileId"`
				Session     string `json:"session"` // 包含完整的 projects 路径
				DownloadURI string `json:"downloadUri"`
			} `json:"fileMetadata"`
		} `json:"listSessionFileMetadataResponse"`
	}
	if err := json.Unmarshal(listRespBody, &listResult); err != nil {
		return "", fmt.Errorf("解析文件元数据失败: %w", err)
	}

	// 查找匹配的文件，获取完整 session 路径
	var fullSession string
	for _, meta := range listResult.ListSessionFileMetadataResponse.FileMetadata {
		if meta.FileID == fileId {
			fullSession = meta.Session // 如: projects/372889301682/locations/global/collections/...
			break
		}
	}

	if fullSession == "" {
		return "", fmt.Errorf("未找到 fileId=%s 的文件信息", fileId)
	}

	downloadURL := fmt.Sprintf("https://biz-discoveryengine.googleapis.com/download/v1alpha/%s:downloadFile?fileId=%s&alt=media", fullSession, fileId)
	downloadReq, _ := http.NewRequest("GET", downloadURL, nil)
	for k, v := range getCommonHeaders(jwt, origAuth) {
		downloadReq.Header.Set(k, v)
	}

	downloadResp, err := httpClient.Do(downloadReq)
	if err != nil {
		return "", fmt.Errorf("下载图片失败: %w", err)
	}
	defer downloadResp.Body.Close()

	imgBody, _ := readResponseBody(downloadResp)

	if downloadResp.StatusCode != 200 {
		return "", fmt.Errorf("下载图片失败: HTTP %d: %s", downloadResp.StatusCode, string(imgBody))
	}

	// 响应是原始二进制图片数据，需要转为 base64
	return base64.StdEncoding.EncodeToString(imgBody), nil
}

// 将图片转换为 Markdown 格式的 data URI
func formatImageAsMarkdown(mimeType, base64Data string) string {
	return fmt.Sprintf("![image](data:%s;base64,%s)", mimeType, base64Data)
}

// 媒体信息（图片/视频）
type MediaInfo struct {
	MimeType  string
	Data      string // base64 数据
	URL       string // 原始 URL（如果有）
	IsURL     bool   // 是否使用 URL 直接上传
	MediaType string // "image" 或 "video"
}

// 别名，保持向后兼容
type ImageInfo = MediaInfo

// 解析消息内容，支持文本、图片和视频
func parseMessageContent(msg Message) (string, []MediaInfo) {
	var textContent string
	var medias []MediaInfo

	switch content := msg.Content.(type) {
	case string:
		textContent = content
	case []interface{}:
		for _, part := range content {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				continue
			}

			partType, _ := partMap["type"].(string)
			switch partType {
			case "text":
				if text, ok := partMap["text"].(string); ok {
					textContent += text
				}
			case "image_url":
				if imgURL, ok := partMap["image_url"].(map[string]interface{}); ok {
					if urlStr, ok := imgURL["url"].(string); ok {
						media := parseMediaURL(urlStr, "image")
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			case "video_url":
				// 支持视频 URL
				if videoURL, ok := partMap["video_url"].(map[string]interface{}); ok {
					if urlStr, ok := videoURL["url"].(string); ok {
						media := parseMediaURL(urlStr, "video")
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			case "file":
				// 支持通用文件类型
				if fileData, ok := partMap["file"].(map[string]interface{}); ok {
					if urlStr, ok := fileData["url"].(string); ok {
						mediaType := "image" // 默认图片
						if mime, ok := fileData["mime_type"].(string); ok {
							if strings.HasPrefix(mime, "video/") {
								mediaType = "video"
							}
						}
						media := parseMediaURL(urlStr, mediaType)
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			}
		}
	}

	return textContent, medias
}

// 解析媒体 URL（图片或视频）
func parseMediaURL(urlStr, defaultType string) *MediaInfo {
	// 处理 base64 数据
	if strings.HasPrefix(urlStr, "data:") {
		// data:image/jpeg;base64,/9j/4AAQ... 或 data:video/mp4;base64,...
		parts := strings.SplitN(urlStr, ",", 2)
		if len(parts) != 2 {
			return nil
		}

		base64Data := parts[1]
		var mediaType string
		var mimeType string

		// 检测媒体类型
		if strings.Contains(parts[0], "video/") {
			mediaType = "video"
			// 视频格式处理
			if strings.Contains(parts[0], "video/mp4") {
				mimeType = "video/mp4"
			} else if strings.Contains(parts[0], "video/webm") {
				mimeType = "video/webm"
			} else if strings.Contains(parts[0], "video/quicktime") || strings.Contains(parts[0], "video/mov") {
				// MOV 格式，尝试作为 mp4 上传
				mimeType = "video/mp4"
				log.Printf("ℹ️ MOV 视频将作为 MP4 上传")
			} else if strings.Contains(parts[0], "video/avi") || strings.Contains(parts[0], "video/x-msvideo") {
				mimeType = "video/mp4"
				log.Printf("ℹ️ AVI 视频将作为 MP4 上传")
			} else {
				// 其他视频格式默认作为 mp4
				mimeType = "video/mp4"
				log.Printf("ℹ️ 未知视频格式 %s 将作为 MP4 上传", parts[0])
			}
		} else {
			mediaType = "image"
			// 图片格式处理
			if strings.Contains(parts[0], "image/png") {
				mimeType = "image/png"
			} else if strings.Contains(parts[0], "image/jpeg") {
				mimeType = "image/jpeg"
			} else {
				// 其他图片格式需要转换为 PNG
				converted, err := convertBase64ToPNG(base64Data)
				if err != nil {
					log.Printf("⚠️ %s base64 转换失败: %v", parts[0], err)
					mimeType = "image/jpeg" // 回退
				} else {
					log.Printf("✅ %s base64 已转换为 PNG", parts[0])
					base64Data = converted
					mimeType = "image/png"
				}
			}
		}

		return &MediaInfo{
			MimeType:  mimeType,
			Data:      base64Data,
			IsURL:     false,
			MediaType: mediaType,
		}
	}

	// URL 媒体 - 优先尝试直接使用 URL 上传
	mediaType := defaultType
	lowerURL := strings.ToLower(urlStr)
	if strings.HasSuffix(lowerURL, ".mp4") || strings.HasSuffix(lowerURL, ".webm") ||
		strings.HasSuffix(lowerURL, ".mov") || strings.HasSuffix(lowerURL, ".avi") ||
		strings.HasSuffix(lowerURL, ".mkv") || strings.HasSuffix(lowerURL, ".m4v") {
		mediaType = "video"
	}

	return &MediaInfo{
		URL:       urlStr,
		IsURL:     true,
		MediaType: mediaType,
	}
}

func downloadImage(urlStr string) (string, string, error) {
	return downloadMedia(urlStr, "image")
}

// downloadMedia 下载媒体文件（图片或视频）
func downloadMedia(urlStr, mediaType string) (string, string, error) {
	resp, err := httpClient.Get(urlStr)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	// 检查上游返回的状态码
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", "", fmt.Errorf("UPSTREAM_%d: 上游返回状态码 %d 多媒体下载失败", resp.StatusCode, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("UPSTREAM_%d: 上游返回状态码 %d", resp.StatusCode, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	mimeType := resp.Header.Get("Content-Type")

	if mediaType == "video" || strings.HasPrefix(mimeType, "video/") {
		// 视频处理
		if mimeType == "" {
			mimeType = "video/mp4"
		}
		// 规范化视频 MIME 类型
		mimeType = normalizeVideoMimeType(mimeType)
		return base64.StdEncoding.EncodeToString(data), mimeType, nil
	}

	// 图片处理
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	needConvert := !strings.Contains(mimeType, "jpeg") && !strings.Contains(mimeType, "png")
	if needConvert {
		converted, err := convertToPNG(data)
		if err != nil {
			log.Printf("⚠️ %s 转换失败: %v，尝试原格式", mimeType, err)
		} else {
			log.Printf("✅ %s 已转换为 PNG", mimeType)
			return base64.StdEncoding.EncodeToString(converted), "image/png", nil
		}
	}

	return base64.StdEncoding.EncodeToString(data), mimeType, nil
}

// normalizeVideoMimeType 规范化视频 MIME 类型
func normalizeVideoMimeType(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "mp4"):
		return "video/mp4"
	case strings.Contains(mimeType, "webm"):
		return "video/webm"
	case strings.Contains(mimeType, "quicktime"), strings.Contains(mimeType, "mov"):
		log.Printf("ℹ️ MOV 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "avi"), strings.Contains(mimeType, "x-msvideo"):
		log.Printf("ℹ️ AVI 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "x-matroska"), strings.Contains(mimeType, "mkv"):
		log.Printf("ℹ️ MKV 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "3gpp"):
		return "video/3gpp"
	default:
		log.Printf("ℹ️ 未知视频格式 %s 将作为 MP4 上传", mimeType)
		return "video/mp4"
	}
}

// convertToPNG 将图片转换为 PNG 格式
func convertToPNG(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码图片失败: %w", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("编码 PNG 失败: %w", err)
	}

	return buf.Bytes(), nil
}

// convertBase64ToPNG 将 base64 图片转换为 PNG
func convertBase64ToPNG(base64Data string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("解码 base64 失败: %w", err)
	}

	converted, err := convertToPNG(data)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(converted), nil
}

const maxRetries = 3

// convertMessagesToPrompt 将多轮对话转换为Gemini格式的prompt
// extractSystemPrompt 提取并返回系统提示词
func extractSystemPrompt(messages []Message) string {
	for _, msg := range messages {
		if msg.Role == "system" {
			text, _ := parseMessageContent(msg)
			return text
		}
	}
	return ""
}

// convertMessagesToPrompt 将多轮对话转换为带系统提示词的prompt
// 支持OpenAI/Claude/Gemini格式的messages
func convertMessagesToPrompt(messages []Message) string {
	var dialogParts []string
	var systemPrompt string

	for _, msg := range messages {
		text, _ := parseMessageContent(msg)
		if text == "" && msg.Role != "assistant" {
			continue
		}

		switch msg.Role {
		case "system":
			// 支持多个system消息拼接
			if systemPrompt != "" {
				systemPrompt += "\n" + text
			} else {
				systemPrompt = text
			}
		case "user", "human": // Claude使用human
			dialogParts = append(dialogParts, fmt.Sprintf("Human: %s", text))
		case "assistant":
			// 检查是否有工具调用
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					dialogParts = append(dialogParts, fmt.Sprintf("Assistant: [调用工具 %s(%s)]", tc.Function.Name, tc.Function.Arguments))
				}
			} else if text != "" {
				dialogParts = append(dialogParts, fmt.Sprintf("Assistant: %s", text))
			}
		case "tool", "tool_result": // Claude使用tool_result
			dialogParts = append(dialogParts, fmt.Sprintf("Tool Result [%s]: %s", msg.Name, text))
		}
	}

	// 组合最终prompt，系统提示词使用更强的格式
	var result strings.Builder
	if systemPrompt != "" {
		// 使用更明确的系统提示词格式，确保生效
		result.WriteString("<system>\n")
		result.WriteString(systemPrompt)
		result.WriteString("\n</system>\n\n")
	}
	if len(dialogParts) > 0 {
		result.WriteString(strings.Join(dialogParts, "\n\n"))
	}
	// 添加Assistant前缀引导回复
	result.WriteString("\n\nAssistant:")
	return result.String()
}

// buildToolsSpec 将OpenAI格式的工具定义转换为Gemini的toolsSpec
func buildToolsSpec(tools []ToolDef, isImageModel, isVideoModel, isSearchModel bool) map[string]interface{} {
	toolsSpec := make(map[string]interface{})

	// 基础工具
	if isImageModel {
		toolsSpec["imageGenerationSpec"] = map[string]interface{}{}
	} else if isVideoModel {
		toolsSpec["videoGenerationSpec"] = map[string]interface{}{}
	} else if isSearchModel {
		// 搜索模型只启用 webGroundingSpec
		toolsSpec["webGroundingSpec"] = map[string]interface{}{}
	} else {
		// 普通模型启用所有内置工具
		toolsSpec["webGroundingSpec"] = map[string]interface{}{}
		toolsSpec["toolRegistry"] = "default_tool_registry"
		toolsSpec["imageGenerationSpec"] = map[string]interface{}{}
		toolsSpec["videoGenerationSpec"] = map[string]interface{}{}
	}

	// 如果有自定义工具，添加functionDeclarations
	if len(tools) > 0 {
		var functionDeclarations []map[string]interface{}
		for _, tool := range tools {
			if tool.Type == "function" {
				funcDecl := map[string]interface{}{
					"name":        tool.Function.Name,
					"description": tool.Function.Description,
				}
				if len(tool.Function.Parameters) > 0 {
					funcDecl["parameters"] = tool.Function.Parameters
				}
				functionDeclarations = append(functionDeclarations, funcDecl)
			}
		}
		if len(functionDeclarations) > 0 {
			toolsSpec["functionDeclarations"] = functionDeclarations
		}
	}

	return toolsSpec
}

// extractToolCalls 从Gemini响应中提取工具调用
func extractToolCalls(dataList []map[string]interface{}) []ToolCall {
	var toolCalls []ToolCall

	for _, data := range dataList {
		streamResp, ok := data["streamAssistResponse"].(map[string]interface{})
		if !ok {
			continue
		}
		answer, ok := streamResp["answer"].(map[string]interface{})
		if !ok {
			continue
		}
		replies, ok := answer["replies"].([]interface{})
		if !ok {
			continue
		}

		for _, reply := range replies {
			replyMap, ok := reply.(map[string]interface{})
			if !ok {
				continue
			}
			groundedContent, ok := replyMap["groundedContent"].(map[string]interface{})
			if !ok {
				continue
			}
			content, ok := groundedContent["content"].(map[string]interface{})
			if !ok {
				continue
			}

			// 检查functionCall
			if fc, ok := content["functionCall"].(map[string]interface{}); ok {
				name, _ := fc["name"].(string)
				args, _ := fc["args"].(map[string]interface{})
				argsBytes, _ := json.Marshal(args)

				toolCalls = append(toolCalls, ToolCall{
					ID:   "call_" + uuid.New().String()[:8],
					Type: "function",
					Function: FunctionCall{
						Name:      name,
						Arguments: string(argsBytes),
					},
				})
			}
		}
	}

	return toolCalls
}

// needsConversationContext 检查是否需要对话上下文（多轮对话）
func needsConversationContext(messages []Message) bool {
	// 检查是否有多轮对话标志：存在assistant或tool消息
	for _, msg := range messages {
		if msg.Role == "assistant" || msg.Role == "tool" || msg.Role == "tool_result" {
			return true
		}
	}
	return false
}
func streamChat(c *gin.Context, req ChatRequest) {
	chatID := "chatcmpl-" + uuid.New().String()
	createdTime := time.Now().Unix()
	clientIP := c.ClientIP()
	// 入站日志
	log.Printf("📥 [%s] 请求: model=%s ", clientIP, req.Model)
	// 解析消息：支持多轮对话拼接和系统提示词
	var textContent string
	var images []MediaInfo
	// 提取系统提示词
	systemPrompt := extractSystemPrompt(req.Messages)
	if needsConversationContext(req.Messages) {
		// 多轮对话：拼接所有消息（包含system）
		textContent = convertMessagesToPrompt(req.Messages)
		// 只从最后一条用户消息提取图片
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" || req.Messages[i].Role == "human" {
				_, images = parseMessageContent(req.Messages[i])
				break
			}
		}
	} else {
		// 简单情况：处理最后一条用户消息
		lastMsg := req.Messages[len(req.Messages)-1]
		userText, userImages := parseMessageContent(lastMsg)
		images = userImages

		// 系统提示词使用强格式拼接，确保生效
		if systemPrompt != "" {
			textContent = fmt.Sprintf("<system>\n%s\n</system>\n\nHuman: %s\n\nAssistant:", systemPrompt, userText)
		} else {
			textContent = userText
		}
	}
	var respBody []byte
	var lastErr error
	var usedAcc *Account
	var usedJWT, usedOrigAuth, usedConfigID, usedSession string

	// 检测是否是可能长时间处理的模型（视频/图片生成）
	isLongRunning := !req.Stream && (strings.Contains(req.Model, "video") ||
		strings.Contains(req.Model, "imagen") ||
		strings.Contains(req.Model, "image"))

	// 对于非流式的长时间任务，启动心跳保持连接
	var heartbeatDone chan struct{}
	if isLongRunning {
		heartbeatDone = make(chan struct{})
		c.Header("Content-Type", "application/json")
		c.Header("Transfer-Encoding", "chunked")
		c.Status(200)
		writer := c.Writer
		flusher, ok := writer.(http.Flusher)
		if ok {
			flusher.Flush() // 先发送头部
		}

		// 启动心跳 goroutine
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// 忽略写入已关闭连接的 panic
				}
			}()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatDone:
					return
				case <-ticker.C:
					// 发送空格作为心跳（不影响 JSON 解析）
					if _, err := writer.Write([]byte(" ")); err != nil {
						return // 写入失败说明连接已关闭
					}
					if flusher, ok := writer.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			}
		}()
	}

	// 确保心跳 goroutine 在函数退出时停止
	defer func() {
		if heartbeatDone != nil {
			select {
			case <-heartbeatDone:
				// 已关闭
			default:
				close(heartbeatDone)
			}
		}
	}()

	for retry := 0; retry < maxRetries; retry++ {
		acc := pool.Next()
		if acc == nil {
			c.JSON(500, gin.H{"error": "没有可用账号"})
			return
		}
		usedAcc = acc
		log.Printf("📤 [%s] 使用账号: %s", clientIP, acc.Data.Email)

		if retry > 0 {
			log.Printf("🔄 第 %d 次重试，切换账号: %s", retry+1, acc.Data.Email)
		}

		jwt, configID, err := acc.GetJWT()
		if err != nil {
			log.Printf("❌ [%s] 获取 JWT 失败: %v", acc.Data.Email, err)
			lastErr = err
			continue
		}

		session, err := createSession(jwt, configID, acc.Data.Authorization)
		if err != nil {
			log.Printf("❌ [%s] 创建 Session 失败: %v", acc.Data.Email, err)
			// 401 错误标记账号需要刷新
			if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "UNAUTHENTICATED") {
				//		pool.MarkNeedsRefresh(acc)
			}
			lastErr = err
			continue
		}

		// 上传媒体文件并获取 fileIds
		var fileIds []string
		uploadFailed := false
		for _, media := range images {
			var fileId string
			var err error

			mediaTypeName := "图片"
			if media.MediaType == "video" {
				mediaTypeName = "视频"
			}

			if media.IsURL {
				// 优先尝试 URL 直接上传
				fileId, err = uploadContextFileByURL(jwt, configID, session, media.URL, acc.Data.Authorization)
				if err != nil {
					// URL 上传失败，回退到下载后上传
					mediaData, mimeType, dlErr := downloadMedia(media.URL, media.MediaType)
					if dlErr != nil {
						log.Printf("⚠️ [%s] %s下载失败: %v", acc.Data.Email, mediaTypeName, dlErr)
						if strings.Contains(dlErr.Error(), "UPSTREAM_401") || strings.Contains(dlErr.Error(), "UPSTREAM_403") {
							c.JSON(500, gin.H{"error": gin.H{
								"message": dlErr.Error(),
								"type":    "upstream_error",
								"code":    "media_download_failed",
							}})
							return
						}
						uploadFailed = true
						break
					}
					fileId, err = uploadContextFile(jwt, configID, session, mimeType, mediaData, acc.Data.Authorization)
				}
			} else {
				fileId, err = uploadContextFile(jwt, configID, session, media.MimeType, media.Data, acc.Data.Authorization)
			}
			if err != nil {
				log.Printf("⚠️ [%s] %s上传失败: %v", acc.Data.Email, mediaTypeName, err)
				uploadFailed = true
				break
			}
			fileIds = append(fileIds, fileId)
		}
		if uploadFailed {
			lastErr = fmt.Errorf("媒体上传失败")
			continue
		}
		// 构建 query parts（只包含文本）
		queryParts := []map[string]interface{}{}
		if textContent != "" {
			queryParts = append(queryParts, map[string]interface{}{"text": textContent})
		}

		// 检查模型类型后缀
		isImageModel := strings.HasSuffix(req.Model, "-image")
		isVideoModel := strings.HasSuffix(req.Model, "-video")
		isSearchModel := strings.HasSuffix(req.Model, "-search")
		actualModel := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(req.Model, "-image"), "-video"), "-search")

		// 构建 toolsSpec（支持自定义工具）
		toolsSpec := buildToolsSpec(req.Tools, isImageModel, isVideoModel, isSearchModel)

		body := map[string]interface{}{
			"configId":         configID,
			"additionalParams": map[string]string{"token": "-"},
			"streamAssistRequest": map[string]interface{}{
				"session":              session,
				"query":                map[string]interface{}{"parts": queryParts},
				"filter":               "",
				"fileIds":              fileIds,
				"answerGenerationMode": "NORMAL",
				"toolsSpec":            toolsSpec,
				"languageCode":         "zh-CN",
				"userMetadata":         map[string]string{"timeZone": "Asia/Shanghai"},
				"assistSkippingMode":   "REQUEST_ASSIST",
			},
		}

		// 设置模型 ID（去掉 -image 后缀）
		if targetModelID, ok := modelMapping[actualModel]; ok && targetModelID != "" {
			body["streamAssistRequest"].(map[string]interface{})["assistGenerationConfig"] = map[string]interface{}{
				"modelId": targetModelID,
			}
		}

		bodyBytes, _ := json.Marshal(body)
		httpReq, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetStreamAssist", bytes.NewReader(bodyBytes))

		for k, v := range getCommonHeaders(jwt, acc.Data.Authorization) {
			httpReq.Header.Set(k, v)
		}

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			log.Printf("❌ [%s] 请求失败: %v", acc.Data.Email, err)
			lastErr = err
			continue
		}

		if resp.StatusCode != 200 {
			body, _ := readResponseBody(resp)
			resp.Body.Close()
			log.Printf("❌ [%s] Google 报错: %d %s (重试 %d/%d)", acc.Data.Email, resp.StatusCode, string(body), retry+1, maxRetries)
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			// 401/403 无权限，标记需要刷新
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				log.Printf("⚠️ [%s] %d 无权限，标记需要刷新", acc.Data.Email, resp.StatusCode)
				pool.MarkNeedsRefresh(acc)
			}
			// 429 限流，延长使用冷却时间（3倍冷却）
			if resp.StatusCode == 429 {
				cooldownTime := UseCooldown * 3
				acc.mu.Lock()
				acc.LastUsed = time.Now().Add(cooldownTime)
				acc.mu.Unlock()
				log.Printf("⏳ [%s] 429 限流，账号进入延长冷却 %v", acc.Data.Email, cooldownTime)
				// 429不计入重试次数，等待后继续尝试其他账号
				pool.MarkUsed(acc, false)
				time.Sleep(1 * time.Second) // 短暂等待后切换账号
				retry--                     // 不计入重试次数
				continue
			}
			pool.MarkUsed(acc, false) // 标记失败
			continue
		}

		// 成功，读取响应
		respBody, _ = readResponseBody(resp)
		resp.Body.Close()

		// 快速检查是否是认证错误响应
		if bytes.Contains(respBody, []byte("uToken")) && !bytes.Contains(respBody, []byte("streamAssistResponse")) {
			log.Printf("⚠️ [%s] 收到认证响应，标记需要刷新", acc.Data.Email)
			pool.MarkNeedsRefresh(acc)
			lastErr = fmt.Errorf("认证失败，需要刷新账号")
			continue
		}

		// 检查是否有实际内容（非空返回）
		hasContent := bytes.Contains(respBody, []byte(`"text"`)) || bytes.Contains(respBody, []byte(`"file"`)) || bytes.Contains(respBody, []byte(`"inlineData"`))
		if !hasContent && bytes.Contains(respBody, []byte(`"thought"`)) {
			// 只有思考内容，没有实际输出，重试
			log.Printf("⚠️ [%s] 响应只有思考内容，无实际输出，重试 (%d/%d)", acc.Data.Email, retry+1, maxRetries)
			lastErr = fmt.Errorf("空返回，只有思考内容")
			continue
		}

		usedJWT = jwt
		usedOrigAuth = acc.Data.Authorization
		usedConfigID = configID
		usedSession = session // 保存创建的 session 作为回退
		usedAcc = acc
		lastErr = nil
		pool.MarkUsed(acc, true) // 标记成功
		break
	}

	if lastErr != nil {
		log.Printf("❌ 所有重试均失败: %v", lastErr)
		c.JSON(500, gin.H{"error": lastErr.Error()})
		return
	}

	_ = usedAcc

	// 检查空响应
	if len(respBody) == 0 {
		log.Printf("❌ 响应为空")
		c.JSON(500, gin.H{"error": "Empty response from Google"})
		return
	}

	// 解析响应：支持多种格式
	var dataList []map[string]interface{}
	var parseErr error

	// 1. 尝试标准 JSON 数组
	if parseErr = json.Unmarshal(respBody, &dataList); parseErr != nil {
		log.Printf("⚠️ JSON 数组解析失败: %v, 响应前100字符: %s", parseErr, string(respBody[:min(100, len(respBody))]))

		// 2. 尝试修复不完整的 JSON 数组
		dataList = parseIncompleteJSONArray(respBody)
		if dataList == nil {
			// 3. 尝试 NDJSON 格式
			log.Printf("⚠️ 尝试 NDJSON 格式...")
			dataList = parseNDJSON(respBody)
		}

		if len(dataList) == 0 {
			// 输出完整响应用于调试
			respStr := string(respBody)
			if len(respStr) > 500 {
				log.Printf("❌ 所有解析方式均失败, 响应长度: %d, 前500字符: %s", len(respBody), respStr[:500])
				log.Printf("❌ 后200字符: %s", respStr[len(respStr)-200:])
			} else {
				log.Printf("❌ 所有解析方式均失败, 响应长度: %d, 完整响应: %s", len(respBody), respStr)
			}
			c.JSON(500, gin.H{"error": "JSON Parse Error"})
			return
		}
		log.Printf("✅ 备用解析成功，共 %d 个对象", len(dataList))
	}

	// 检查是否有有效响应
	if len(dataList) > 0 {
		hasValidResponse := false
		hasFileContent := false
		for _, data := range dataList {
			if streamResp, ok := data["streamAssistResponse"].(map[string]interface{}); ok {
				hasValidResponse = true
				// 检查是否有文件内容
				if answer, ok := streamResp["answer"].(map[string]interface{}); ok {
					if replies, ok := answer["replies"].([]interface{}); ok {
						for _, reply := range replies {
							if replyMap, ok := reply.(map[string]interface{}); ok {
								if gc, ok := replyMap["groundedContent"].(map[string]interface{}); ok {
									if content, ok := gc["content"].(map[string]interface{}); ok {
										if _, ok := content["file"]; ok {
											hasFileContent = true
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if !hasValidResponse {
			log.Printf("⚠️ 响应中没有 streamAssistResponse，响应内容: %v", dataList[0])
		}
		log.Printf("📊 响应统计: %d 个数据块, 有效响应=%v, 包含文件=%v", len(dataList), hasValidResponse, hasFileContent)
	}

	// 从响应中提取 session（用于下载图片）
	var respSession string
	for _, data := range dataList {
		if streamResp, ok := data["streamAssistResponse"].(map[string]interface{}); ok {
			if sessionInfo, ok := streamResp["sessionInfo"].(map[string]interface{}); ok {
				if s, ok := sessionInfo["session"].(string); ok && s != "" {
					respSession = s
					break
				}
			}
		}
	}

	// 如果响应中没有 session，使用请求时创建的 session 作为回退
	if respSession == "" {
		if usedSession != "" {
			log.Printf("⚠️ 响应中未找到 session，使用请求时创建的 session: %s", usedSession)
			respSession = usedSession
		} else {
			log.Printf("⚠️ 响应中未找到 session 且无回退 session，图片/视频下载可能失败")
		}
	} else {
	}

	// 待下载的文件信息
	type PendingFile struct {
		FileID   string
		MimeType string
	}

	if req.Stream {
		// 流式响应：文本/思考实时输出，图片最后处理
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")

		writer := c.Writer
		flusher, _ := writer.(http.Flusher)

		// 发送 role
		chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"role": "assistant"}, nil)
		fmt.Fprintf(writer, "data: %s\n\n", chunk)
		flusher.Flush()

		// 收集待下载的文件和工具调用
		var pendingFiles []PendingFile
		hasToolCalls := false
		for _, data := range dataList {
			streamResp, ok := data["streamAssistResponse"].(map[string]interface{})
			if !ok {
				continue
			}
			answer, ok := streamResp["answer"].(map[string]interface{})
			if !ok {
				continue
			}
			replies, ok := answer["replies"].([]interface{})
			if !ok {
				continue
			}
			for _, reply := range replies {
				replyMap, ok := reply.(map[string]interface{})
				if !ok {
					continue
				}
				groundedContent, ok := replyMap["groundedContent"].(map[string]interface{})
				if !ok {
					continue
				}
				content, ok := groundedContent["content"].(map[string]interface{})
				if !ok {
					continue
				}
				// 检查是否是思考内容
				if thought, ok := content["thought"].(bool); ok && thought {
					if t, ok := content["text"].(string); ok && t != "" {
						chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"reasoning_content": t}, nil)
						fmt.Fprintf(writer, "data: %s\n\n", chunk)
						flusher.Flush()
					}
					continue
				}
				// 输出文本（实时）
				if t, ok := content["text"].(string); ok && t != "" {
					chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": t}, nil)
					fmt.Fprintf(writer, "data: %s\n\n", chunk)
					flusher.Flush()
				}

				// 处理 inlineData（直接有 base64 数据的图片）
				if inlineData, ok := content["inlineData"].(map[string]interface{}); ok {
					mime, _ := inlineData["mimeType"].(string)
					data, _ := inlineData["data"].(string)
					if mime != "" && data != "" {
						imgMarkdown := formatImageAsMarkdown(mime, data)
						chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": imgMarkdown}, nil)
						fmt.Fprintf(writer, "data: %s\n\n", chunk)
						flusher.Flush()
					}
				}

				// 收集需要下载的文件（图片/视频）
				if file, ok := content["file"].(map[string]interface{}); ok {
					fileId, _ := file["fileId"].(string)
					mimeType, _ := file["mimeType"].(string)
					if fileId != "" {
						pendingFiles = append(pendingFiles, PendingFile{FileID: fileId, MimeType: mimeType})
					}
				}
				if fc, ok := content["functionCall"].(map[string]interface{}); ok {
					hasToolCalls = true
					name, _ := fc["name"].(string)
					args, _ := fc["args"].(map[string]interface{})
					argsBytes, _ := json.Marshal(args)

					toolCall := ToolCall{
						ID:   "call_" + uuid.New().String()[:8],
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: string(argsBytes),
						},
					}
					chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{
						"tool_calls": []map[string]interface{}{{
							"index": 0,
							"id":    toolCall.ID,
							"type":  "function",
							"function": map[string]interface{}{
								"name":      toolCall.Function.Name,
								"arguments": toolCall.Function.Arguments,
							},
						}},
					}, nil)
					fmt.Fprintf(writer, "data: %s\n\n", chunk)
					flusher.Flush()
				}
			}
		}
		if len(pendingFiles) > 0 {
			log.Printf("📥 开始下载 %d 个文件...", len(pendingFiles))
			type downloadResult struct {
				Index    int
				Data     string
				MimeType string
				Err      error
			}
			results := make(chan downloadResult, len(pendingFiles))
			var wg sync.WaitGroup
			for i, pf := range pendingFiles {
				wg.Add(1)
				go func(idx int, file PendingFile) {
					defer wg.Done()
					data, err := downloadGeneratedFile(usedJWT, file.FileID, respSession, usedConfigID, usedOrigAuth)
					results <- downloadResult{Index: idx, Data: data, MimeType: file.MimeType, Err: err}
				}(i, pf)
			}
			go func() {
				wg.Wait()
				close(results)
			}()
			downloaded := make([]downloadResult, len(pendingFiles))
			for r := range results {
				downloaded[r.Index] = r
			}

			// 按顺序输出
			for i, r := range downloaded {
				if r.Err != nil {
					log.Printf("❌ 下载文件[%d]失败: %v", i, r.Err)
					continue
				}
				imgMarkdown := formatImageAsMarkdown(r.MimeType, r.Data)
				chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": imgMarkdown}, nil)
				fmt.Fprintf(writer, "data: %s\n\n", chunk)
				flusher.Flush()
			}
		}

		// 发送结束
		finishReason := "stop"
		if hasToolCalls {
			finishReason = "tool_calls"
		}
		finalChunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{}, &finishReason)
		fmt.Fprintf(writer, "data: %s\n\n", finalChunk)
		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// 非流式响应
		var fullContent strings.Builder
		var fullReasoning strings.Builder
		replyCount := 0
		hasFile := false

		for _, data := range dataList {
			streamResp, ok := data["streamAssistResponse"].(map[string]interface{})
			if !ok {
				continue
			}
			answer, ok := streamResp["answer"].(map[string]interface{})
			if !ok {
				continue
			}
			replies, ok := answer["replies"].([]interface{})
			if !ok {
				continue
			}

			for _, reply := range replies {
				replyMap, ok := reply.(map[string]interface{})
				if !ok {
					continue
				}
				replyCount++
				if gc, ok := replyMap["groundedContent"].(map[string]interface{}); ok {
					if content, ok := gc["content"].(map[string]interface{}); ok {
						if _, ok := content["file"]; ok {
							hasFile = true
						}
					}
				}

				text, imageData, imageMime, reasoning := extractContentFromReply(replyMap, usedJWT, respSession, usedConfigID, usedOrigAuth)
				if reasoning != "" {
					fullReasoning.WriteString(reasoning)
				}
				if text != "" {
					fullContent.WriteString(text)
				}
				if imageData != "" && imageMime != "" {
					fullContent.WriteString(formatImageAsMarkdown(imageMime, imageData))
				}
			}
		}
		toolCalls := extractToolCalls(dataList)
		// 调试日志
		log.Printf("📊 非流式响应统计: %d 个 reply, 包含文件=%v, content长度=%d, reasoning长度=%d, 工具调用=%d",
			replyCount, hasFile, fullContent.Len(), fullReasoning.Len(), len(toolCalls))

		// 构建响应消息
		message := gin.H{
			"role":    "assistant",
			"content": fullContent.String(),
		}
		if fullReasoning.Len() > 0 {
			message["reasoning_content"] = fullReasoning.String()
		}
		finishReason := "stop"
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			message["content"] = nil
			finishReason = "tool_calls"
		}

		// 构建最终响应
		response := gin.H{
			"id":      chatID,
			"object":  "chat.completion",
			"created": createdTime,
			"model":   req.Model,
			"choices": []gin.H{{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			}},
			"usage": gin.H{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		}

		// 对于长时间运行的模型，停止心跳后直接写入 JSON
		if isLongRunning && heartbeatDone != nil {
			close(heartbeatDone) // 停止心跳
			jsonBytes, _ := json.Marshal(response)
			c.Writer.Write(jsonBytes)
		} else {
			c.JSON(200, response)
		}
	}
}
func apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(appConfig.APIKeys) == 0 {
			c.Next()
			return
		}
		authHeader := c.GetHeader("Authorization")
		apiKey := ""

		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			apiKey = c.GetHeader("X-API-Key")
		}

		if apiKey == "" {
			c.JSON(401, gin.H{"error": "Missing API key"})
			c.Abort()
			return
		}

		// 验证 API Key
		valid := false
		for _, key := range appConfig.APIKeys {
			if key == apiKey {
				valid = true
				break
			}
		}

		if !valid {
			c.JSON(401, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// runBrowserRefreshMode 有头浏览器刷新模式
func runBrowserRefreshMode(email string) {
	loadAppConfig()
	initHTTPClient()

	// 强制有头模式
	BrowserRefreshHeadless = false
	log.Println("🌐 有头浏览器刷新模式")

	if err := pool.Load(DataDir); err != nil {
		log.Fatalf("❌ 加载账号失败: %v", err)
	}

	if pool.TotalCount() == 0 {
		log.Fatal("❌ 没有可用账号")
	}

	// 查找目标账号
	var targetAcc *Account
	pool.mu.RLock()
	if email != "" {
		// 指定邮箱
		for _, acc := range pool.readyAccounts {
			if acc.Data.Email == email {
				targetAcc = acc
				break
			}
		}
		if targetAcc == nil {
			for _, acc := range pool.pendingAccounts {
				if acc.Data.Email == email {
					targetAcc = acc
					break
				}
			}
		}
	} else {
		// 使用第一个账号
		if len(pool.readyAccounts) > 0 {
			targetAcc = pool.readyAccounts[0]
		} else if len(pool.pendingAccounts) > 0 {
			targetAcc = pool.pendingAccounts[0]
		}
	}
	pool.mu.RUnlock()

	if targetAcc == nil {
		if email != "" {
			log.Fatalf("❌ 找不到账号: %s", email)
		}
		log.Fatal("❌ 没有可用账号")
	}
	result := RefreshCookieWithBrowser(targetAcc, false, Proxy)

	if result.Success {

		if len(result.NewCookies) > 0 {
		}
		if len(result.ResponseHeaders) > 0 {
		}

		// 更新账号数据
		targetAcc.mu.Lock()
		targetAcc.Data.Cookies = result.SecureCookies
		if result.Authorization != "" {
			targetAcc.Data.Authorization = result.Authorization
		}
		if result.ConfigID != "" {
			targetAcc.ConfigID = result.ConfigID
			targetAcc.Data.ConfigID = result.ConfigID
		}
		if result.CSESIDX != "" {
			targetAcc.CSESIDX = result.CSESIDX
			targetAcc.Data.CSESIDX = result.CSESIDX
		}
		// 保存响应头
		if len(result.ResponseHeaders) > 0 {
			targetAcc.Data.ResponseHeaders = result.ResponseHeaders
		}
		targetAcc.mu.Unlock()

		// 保存到文件
		if err := targetAcc.SaveToFile(); err != nil {
			log.Printf("⚠️ 保存失败: %v", err)
		} else {
			log.Printf("💾 已保存到: %s", targetAcc.FilePath)
		}
	} else {
		log.Printf("❌ 刷新失败: %v", result.Error)
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	var refreshEmail string
	var refreshMode bool

	// 解析命令行参数
	for i, arg := range os.Args[1:] {
		switch arg {
		case "--debug", "-d":
			RegisterDebug = true
			log.Println("🔧 调试模式已启用，将保存截图到 data/screenshots/")
		case "--once":
			RegisterOnce = true
			log.Println("🔧 单次运行模式")
		case "--refresh":
			refreshMode = true
			// 检查下一个参数是否是邮箱
			if i+2 < len(os.Args) && !strings.HasPrefix(os.Args[i+2], "-") {
				refreshEmail = os.Args[i+2]
			}
		case "--help", "-h":
			fmt.Println(`用法: ./gemini-gateway [选项]

选项:
  --debug, -d           调试模式，保存注册过程截图
  --once                单次注册模式（调试用）
  --refresh [email]     有头浏览器刷新账号（不指定email则使用第一个账号）
  --help, -h            显示帮助`)
			os.Exit(0)
		}
	}

	// 刷新模式：直接执行浏览器刷新后退出
	if refreshMode {
		runBrowserRefreshMode(refreshEmail)
		return
	}

	loadAppConfig()
	initHTTPClient()
	if err := pool.Load(DataDir); err != nil {
		log.Fatalf("❌ 加载账号失败: %v", err)
	}

	// 检查 CONFIG_ID
	if DefaultConfig != "" {
		log.Printf("✅ 使用默认 configId: %s", DefaultConfig)
	}

	// 检查 API Key 配置
	if len(appConfig.APIKeys) == 0 {
		log.Println("⚠️ 未配置 API Key，API 将无鉴权运行")
	}

	// 启动号池管理
	if appConfig.Pool.RefreshOnStartup {
		pool.StartPoolManager()
	}
	if pool.TotalCount() == 0 {
		needCount := appConfig.Pool.TargetCount
		log.Printf("📝 无账号，启动注册 %d 个...", needCount)
		startRegister(needCount)
	}
	if appConfig.Pool.CheckIntervalMinutes > 0 {
		go poolMaintainer()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Printf("%s %s %d %v", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	})

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "running",
			"service": "business2api",
			"version": "1.0.0",
			"endpoints": gin.H{
				"openai": "/v1/chat/completions",
				"claude": "/v1/messages",
				"gemini": "/v1beta/models/{model}:generateContent",
				"models": "/v1/models",
				"health": "/health",
			},
			"pool": gin.H{
				"ready":   pool.ReadyCount(),
				"pending": pool.PendingCount(),
				"total":   pool.TotalCount(),
			},
		})
	})
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"time":    time.Now().UTC().Format(time.RFC3339),
			"ready":   pool.ReadyCount(),
			"pending": pool.PendingCount(),
		})
	})
	api := r.Group("/")
	api.Use(apiKeyAuth())
	api.GET("/v1/models", func(c *gin.Context) {
		now := time.Now().Unix()
		var models []gin.H
		for _, m := range FixedModels {
			models = append(models, gin.H{
				"id":         m,
				"object":     "model",
				"created":    now,
				"owned_by":   "google",
				"permission": []interface{}{},
			})
		}
		c.JSON(200, gin.H{"object": "list", "data": models})
	})

	api.POST("/v1/chat/completions", func(c *gin.Context) {
		var req ChatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		if req.Model == "" {
			req.Model = FixedModels[0]
		}

		streamChat(c, req)
	})
	api.POST("/v1/messages", handleClaudeMessages)
	api.POST("/v1beta/models/*action", handleGeminiGenerate)
	api.POST("/v1/models/*action", handleGeminiGenerate)
	admin := r.Group("/admin")
	admin.Use(apiKeyAuth())
	admin.POST("/register", func(c *gin.Context) {
		var req struct {
			Count int `json:"count"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Count <= 0 {
			req.Count = appConfig.Pool.TargetCount - pool.Count()
		}
		if req.Count <= 0 {
			c.JSON(200, gin.H{"message": "账号数量已足够", "count": pool.Count()})
			return
		}
		if err := startRegister(req.Count); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"message": "注册已启动", "target": req.Count})
	})
	admin.POST("/refresh", func(c *gin.Context) {
		pool.Load(DataDir)
		c.JSON(200, gin.H{
			"message": "刷新完成",
			"ready":   pool.ReadyCount(),
			"pending": pool.PendingCount(),
		})
	})

	// 获取状态（增强版）
	admin.GET("/status", func(c *gin.Context) {
		stats := pool.Stats()
		stats["target"] = appConfig.Pool.TargetCount
		stats["min"] = appConfig.Pool.MinCount
		stats["is_registering"] = atomic.LoadInt32(&isRegistering) == 1
		stats["register_stats"] = registerStats.Get()
		c.JSON(200, stats)
	})

	// 列出所有账号
	admin.GET("/accounts", func(c *gin.Context) {
		accounts := pool.ListAccounts()
		c.JSON(200, gin.H{
			"count":    len(accounts),
			"accounts": accounts,
		})
	})

	// 强制刷新所有账号
	admin.POST("/force-refresh", func(c *gin.Context) {
		count := pool.ForceRefreshAll()
		c.JSON(200, gin.H{
			"message": "已触发强制刷新",
			"count":   count,
		})
	})

	// 更新冷却配置
	admin.POST("/config/cooldown", func(c *gin.Context) {
		var req struct {
			RefreshCooldownSec int `json:"refresh_cooldown_sec"`
			UseCooldownSec     int `json:"use_cooldown_sec"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		SetCooldowns(req.RefreshCooldownSec, req.UseCooldownSec)
		c.JSON(200, gin.H{
			"message":              "冷却配置已更新",
			"refresh_cooldown_sec": int(RefreshCooldown.Seconds()),
			"use_cooldown_sec":     int(UseCooldown.Seconds()),
		})
	})

	// 手动触发浏览器刷新指定账号
	admin.POST("/browser-refresh", func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		if req.Email == "" {
			c.JSON(400, gin.H{"error": "需要提供 email"})
			return
		}

		// 查找账号
		accounts := pool.ListAccounts()
		var targetAcc *Account
		pool.mu.RLock()
		for _, acc := range pool.readyAccounts {
			if acc.Data.Email == req.Email {
				targetAcc = acc
				break
			}
		}
		if targetAcc == nil {
			for _, acc := range pool.pendingAccounts {
				if acc.Data.Email == req.Email {
					targetAcc = acc
					break
				}
			}
		}
		pool.mu.RUnlock()

		if targetAcc == nil {
			c.JSON(404, gin.H{"error": "账号未找到", "email": req.Email})
			return
		}

		// 执行浏览器刷新
		go func() {
			log.Printf(" 手动触发浏览器刷新: %s", req.Email)
			result := RefreshCookieWithBrowser(targetAcc, BrowserRefreshHeadless, Proxy)
			if result.Success {
				targetAcc.mu.Lock()
				targetAcc.Data.Cookies = result.SecureCookies
				if result.CSESIDX != "" {
					targetAcc.CSESIDX = result.CSESIDX
					targetAcc.Data.CSESIDX = result.CSESIDX
				}
				targetAcc.FailCount = 0
				targetAcc.mu.Unlock()

				if err := targetAcc.SaveToFile(); err != nil {
					log.Printf(" [%s] 保存刷新后的Cookie失败: %v", req.Email, err)
				}
				pool.MarkNeedsRefresh(targetAcc)
				log.Printf(" 手动浏览器刷新成功: %s", req.Email)
			} else {
				log.Printf(" 手动浏览器刷新失败: %s - %v", req.Email, result.Error)
			}
		}()

		c.JSON(200, gin.H{
			"message": "浏览器刷新已触发",
			"email":   req.Email,
		})
		_ = accounts // 避免未使用警告
	})

	// 切换浏览器刷新开关
	admin.POST("/config/browser-refresh", func(c *gin.Context) {
		var req struct {
			Enable   *bool `json:"enable"`
			Headless *bool `json:"headless"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		if req.Enable != nil {
			EnableBrowserRefresh = *req.Enable
		}
		if req.Headless != nil {
			BrowserRefreshHeadless = *req.Headless
		}

		c.JSON(200, gin.H{
			"message":  "浏览器刷新配置已更新",
			"enable":   EnableBrowserRefresh,
			"headless": BrowserRefreshHeadless,
		})
	})

	log.Printf(" 服务启动于 %s，账号: ready=%d, pending=%d", ListenAddr, pool.ReadyCount(), pool.PendingCount())
	if err := r.Run(ListenAddr); err != nil {
		log.Fatalf(" 服务启动失败: %v", err)
	}
}
