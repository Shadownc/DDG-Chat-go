package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type Config struct {
	APIPrefix     string
	MaxRetryCount int
	RetryDelay    time.Duration
	FakeHeaders   map[string]string
	ProxyURL      string
}

var config Config

func init() {
	godotenv.Load()
	config = Config{
		APIPrefix:     getEnv("API_PREFIX", "/"),
			MaxRetryCount: getIntEnv("MAX_RETRY_COUNT", 3),
			RetryDelay:    getDurationEnv("RETRY_DELAY", 5000),
			ProxyURL:      getEnv("PROXY_URL", ""),
			FakeHeaders: map[string]string{
				"Accept":             "*/*",
				"Accept-Encoding":    "gzip, deflate, br, zstd",
				"Accept-Language":    "zh-CN,zh;q=0.9",
				"Origin":             "https://duckduckgo.com/",
				"Cookie":             "dcm=3; dcs=1",
				"Priority":           "u=1, i",
				"Referer":            "https://duckduckgo.com/",
				"Sec-Ch-Ua":          `"Chromium";v="134", "Not:A-Brand";v="24", "Google Chrome";v="134"`,
				"Sec-Ch-Ua-Mobile":   "?0",
				"Sec-Ch-Ua-Platform": `"Windows"`,
				"Sec-Fetch-Dest":     "empty",
				"Sec-Fetch-Mode":     "cors",
				"Sec-Fetch-Site":     "same-origin",
				"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36",
			},
	}
}

func main() {
	r := gin.Default()
	
	// Try to disable trusting proxies if the version supports it
	// Uncomment this if your Gin version supports it
	// r.ForwardedByClientIP = false
	
	r.Use(corsMiddleware())

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "API 服务运行中~"})
	})

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	r.GET(config.APIPrefix+"/v1/models", func(c *gin.Context) {
		models := []gin.H{
			{"id": "gpt-4o-mini", "object": "model", "owned_by": "ddg"},
			{"id": "claude-3-haiku", "object": "model", "owned_by": "ddg"},
			{"id": "llama-3.3-70b", "object": "model", "owned_by": "ddg"},
			{"id": "mistral-small", "object": "model", "owned_by": "ddg"},
			{"id": "o3-mini", "object": "model", "owned_by": "ddg"},
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
	})

	r.POST(config.APIPrefix+"/v1/chat/completions", handleCompletion)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8787"
	}
	r.Run(":" + port)
}

func handleCompletion(c *gin.Context) {
	apiKey := os.Getenv("APIKEY")
	authorizationHeader := c.GetHeader("Authorization")

	if apiKey != "" {
		if authorizationHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未提供 APIKEY"})
			return
		} else if !strings.HasPrefix(authorizationHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "APIKEY 格式错误"})
			return
		} else {
			providedToken := strings.TrimPrefix(authorizationHeader, "Bearer ")
			if providedToken != apiKey {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "APIKEY无效"})
				return
			}
		}
	}

	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	model := convertModel(req.Model)
	content := prepareMessages(req.Messages)

	var retryCount = 0
	var lastError error

	for retryCount <= config.MaxRetryCount {
		if retryCount > 0 {
			log.Printf("重试中... 次数: %d", retryCount)
			time.Sleep(config.RetryDelay)
		}

		token, vqdHash, err := requestTokenAndHash()
		if err != nil {
			lastError = fmt.Errorf("无法获取token: %v", err)
			retryCount++
			continue
		}

		reqBody := map[string]interface{}{
			"model": model,
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": content,
				},
			},
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("请求体序列化失败: %v", err)})
			return
		}

		upstreamReq, err := http.NewRequest("POST", "https://duckduckgo.com/duckchat/v1/chat", strings.NewReader(string(body)))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("创建请求失败: %v", err)})
			return
		}

		for k, v := range config.FakeHeaders {
			upstreamReq.Header.Set(k, v)
		}
		upstreamReq.Header.Set("x-vqd-4", token)
		if vqdHash != "" {
			upstreamReq.Header.Set("x-vqd-hash-1", vqdHash)
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Accept", "text/event-stream")

		client := createHTTPClient(30 * time.Second)
		resp, err := client.Do(upstreamReq)
		if err != nil {
			lastError = fmt.Errorf("请求失败: %v", err)
			retryCount++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			lastError = fmt.Errorf("非200响应: %d, 内容: %s", resp.StatusCode, string(bodyBytes))
			retryCount++
			continue
		}

		// 处理响应
		if err := handleResponse(c, resp, req.Stream, model); err != nil {
			lastError = err
			retryCount++
			continue
		}

		return
	}

	// 如果所有重试都失败了
	c.JSON(http.StatusInternalServerError, gin.H{"error": lastError.Error()})
}

func handleResponse(c *gin.Context, resp *http.Response, isStream bool, model string) error {
	if isStream {
		return handleStreamResponse(c, resp, model)
	}
	return handleNonStreamResponse(c, resp, model)
}

func handleStreamResponse(c *gin.Context, resp *http.Response, model string) error {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return errors.New("Streaming not supported")
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("读取流式响应失败: %v", err)
			}
			break
		}

		if strings.HasPrefix(line, "data: ") {
			line = strings.TrimPrefix(line, "data: ")
			line = strings.TrimSpace(line)

			if line == "[DONE]" {
				response := map[string]interface{}{
					"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{
						{
							"index":         0,
							"finish_reason": "stop",
						},
					},
				}
				sseData, _ := json.Marshal(response)
				sseMessage := fmt.Sprintf("data: %s\n\n", sseData)
				if _, err := c.Writer.Write([]byte(sseMessage)); err != nil {
					return fmt.Errorf("写入响应失败: %v", err)
				}
				flusher.Flush()
				break
			}

			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				log.Printf("解析响应行失败: %v", err)
				continue
			}

			if chunk["action"] != "success" {
				continue
			}

			if msg, exists := chunk["message"]; exists && msg != nil {
				if msgStr, ok := msg.(string); ok {
					response := map[string]interface{}{
						"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{
							{
								"index": 0,
								"delta": map[string]string{
									"content": msgStr,
								},
								"finish_reason": nil,
							},
						},
					}
					sseData, _ := json.Marshal(response)
					sseMessage := fmt.Sprintf("data: %s\n\n", sseData)

					if _, err := c.Writer.Write([]byte(sseMessage)); err != nil {
						return fmt.Errorf("写入响应失败: %v", err)
					}
					flusher.Flush()
				}
			}
		}
	}
	return nil
}

func handleNonStreamResponse(c *gin.Context, resp *http.Response, model string) error {
	var fullResponse strings.Builder
	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("读取响应失败: %v", err)
		}

		if strings.HasPrefix(line, "data: ") {
			line = strings.TrimPrefix(line, "data: ")
			line = strings.TrimSpace(line)

			if line == "[DONE]" {
				break
			}

			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				log.Printf("解析响应行失败: %v", err)
				continue
			}

			if chunk["action"] != "success" {
				continue
			}

			if msg, exists := chunk["message"]; exists && msg != nil {
				if msgStr, ok := msg.(string); ok {
					fullResponse.WriteString(msgStr)
				}
			}
		}
	}

	response := map[string]interface{}{
		"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"usage": map[string]int{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
		"choices": []map[string]interface{}{
			{
				"message": map[string]string{
					"role":    "assistant",
					"content": fullResponse.String(),
				},
				"index": 0,
			},
		},
	}

	c.JSON(http.StatusOK, response)
	return nil
}

func requestToken() (string, error) {
	token, _, err := requestTokenAndHash()
	return token, err
}

func requestTokenAndHash() (string, string, error) {
	// Create a client with proper settings
	client := createHTTPClient(30 * time.Second)
	
	// First, visit the DuckDuckGo main page
	req, err := http.NewRequest("GET", "https://duckduckgo.com/", nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create homepage request: %v", err)
	}
	
	for k, v := range config.FakeHeaders {
		req.Header.Set(k, v)
	}
	
	log.Println("Sending request to DuckDuckGo homepage")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("homepage request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("homepage request returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	
	// Read homepage content to extract vqd-related JavaScript
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read homepage: %v", err)
	}
	
	bodyStr := string(bodyBytes)
	
	// Method 1: Try to extract the vqd directly from the HTML attributes
	// DuckDuckGo often embeds this in data attributes or JavaScript variables
	vqdRegex := regexp.MustCompile(`vqd=["']([^"']+)["']`)
	matches := vqdRegex.FindStringSubmatch(bodyStr)
	
	if len(matches) < 2 {
		// Try alternate regex patterns if the first one didn't match
		alternatePatterns := []string{
			`vqd[:=]["']([^"']+)["']`,
			`"vqd":"([^"]+)"`,
			`'vqd':'([^']+)'`,
		}
		
		for _, pattern := range alternatePatterns {
			r := regexp.MustCompile(pattern)
			matches = r.FindStringSubmatch(bodyStr)
			if len(matches) >= 2 {
				break
			}
		}
	}
	
	if len(matches) >= 2 {
		token := matches[1]
		log.Printf("Successfully extracted vqd token: %s", token)
		return token, "", nil
	}
	
	// Method 2: Get the JavaScript file that contains the token generation logic
	jsURLRegex := regexp.MustCompile(`(\/dist\/[^"']+\.js)`)
	jsMatches := jsURLRegex.FindAllStringSubmatch(bodyStr, -1)
	
	for _, match := range jsMatches {
		if len(match) < 2 {
			continue
		}
		
		jsURL := "https://duckduckgo.com" + match[1]
		log.Printf("Trying to get vqd from JavaScript file: %s", jsURL)
		
		jsReq, err := http.NewRequest("GET", jsURL, nil)
		if err != nil {
			log.Printf("Failed to create request for JS file: %v", err)
			continue
		}
		
		for k, v := range config.FakeHeaders {
			jsReq.Header.Set(k, v)
		}
		
		jsResp, err := client.Do(jsReq)
		if err != nil {
			log.Printf("Failed to get JS file: %v", err)
			continue
		}
		
		jsBytes, err := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()
		
		if err != nil {
			log.Printf("Failed to read JS file: %v", err)
			continue
		}
		
		jsContent := string(jsBytes)
		
		// Look for vqd token patterns in the JS file
		vqdInJSRegex := regexp.MustCompile(`vqd\s*[:=]\s*["']([^"']+)["']`)
		jsMatches := vqdInJSRegex.FindStringSubmatch(jsContent)
		
		if len(jsMatches) >= 2 {
			token := jsMatches[1]
			log.Printf("Found vqd token in JS file: %s", token)
			return token, "", nil
		}
		
		// Try another pattern specific to the format in the JS file
		vqdInJSRegex2 := regexp.MustCompile(`"vqd"\s*:\s*"([^"]+)"`)
		jsMatches2 := vqdInJSRegex2.FindStringSubmatch(jsContent)
		
		if len(jsMatches2) >= 2 {
			token := jsMatches2[1]
			log.Printf("Found vqd token in JS file (pattern 2): %s", token)
			return token, "", nil
		}
	}
	
	// Method 3 (Fallback): Try to request duck.js or related files which might contain the token
	fallbackURLs := []string{
		"https://duckduckgo.com/duck.js",
		"https://duckduckgo.com/chat.js",
		"https://duckduckgo.com/d.js",
	}
	
	for _, url := range fallbackURLs {
		fallbackReq, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("Failed to create request for fallback URL %s: %v", url, err)
			continue
		}
		
		for k, v := range config.FakeHeaders {
			fallbackReq.Header.Set(k, v)
		}
		
		fallbackResp, err := client.Do(fallbackReq)
		if err != nil {
			log.Printf("Failed to get fallback URL %s: %v", url, err)
			continue
		}
		
		fallbackBytes, err := io.ReadAll(fallbackResp.Body)
		fallbackResp.Body.Close()
		
		if err != nil {
			log.Printf("Failed to read fallback URL %s: %v", url, err)
			continue
		}
		
		fallbackContent := string(fallbackBytes)
		
		// Try multiple regex patterns to find the vqd token
		patterns := []string{
			`vqd\s*[:=]\s*["']([^"']+)["']`,
			`"vqd"\s*:\s*"([^"]+)"`,
			`'vqd'\s*:\s*'([^']+)'`,
		}
		
		for _, pattern := range patterns {
			r := regexp.MustCompile(pattern)
			m := r.FindStringSubmatch(fallbackContent)
			if len(m) >= 2 {
				token := m[1]
				log.Printf("Found vqd token in fallback URL %s: %s", url, token)
				return token, "", nil
			}
		}
	}
	
	// Last resort: try the duckchat/v1/status endpoint which we know returns the token in headers
	statusReq, err := http.NewRequest("GET", "https://duckduckgo.com/duckchat/v1/status", nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create status request: %v", err)
	}
	
	for k, v := range config.FakeHeaders {
		statusReq.Header.Set(k, v)
	}
	statusReq.Header.Set("x-vqd-accept", "1")
	
	log.Println("Trying duckchat/v1/status as last resort")
	statusResp, err := client.Do(statusReq)
	if err != nil {
		return "", "", fmt.Errorf("status request failed: %v", err)
	}
	defer statusResp.Body.Close()
	
	// Check X-VQD-4 header
	token := statusResp.Header.Get("x-vqd-4")
	if token != "" {
		log.Printf("Found vqd token in status response header: %s", token)
		return token, "", nil
	}
	
	return "", "", errors.New("could not find vqd token using any method")
}

func prepareMessages(messages []struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}) string {
	var contentBuilder strings.Builder

	for _, msg := range messages {
		// Determine the role - 'system' becomes 'user'
		role := msg.Role
		if role == "system" {
			role = "user"
		}

		// Process the content as string
		contentStr := ""
		switch v := msg.Content.(type) {
		case string:
			contentStr = v
		case []interface{}:
			for _, item := range v {
				if itemMap, ok := item.(map[string]interface{}); ok {
					if text, exists := itemMap["text"].(string); exists {
						contentStr += text
					}
				}
			}
		default:
			contentStr = fmt.Sprintf("%v", msg.Content)
		}

		// Append the role and content to the builder
		contentBuilder.WriteString(fmt.Sprintf("%s:%s;\r\n", role, contentStr))
	}

	return contentBuilder.String()
}

func convertModel(inputModel string) string {
	switch strings.ToLower(inputModel) {
	case "claude-3-haiku":
		return "claude-3-haiku-20240307"
	case "llama-3.3-70b":
		return "meta-llama/Llama-3.3-70B-Instruct-Turbo"
	case "mistral-small":
		return "mistralai/Mistral-Small-24B-Instruct-2501"
	case "o3-mini":
		return "o3-mini"
	default:
		return "gpt-4o-mini"
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "*")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		var intValue int
		fmt.Sscanf(value, "%d", &intValue)
		return intValue
	}
	return fallback
}

func getDurationEnv(key string, fallback int) time.Duration {
	return time.Duration(getIntEnv(key, fallback)) * time.Millisecond
}

func createHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{
		Timeout: timeout,
	}
	
	if config.ProxyURL != "" {
		proxyURL, err := url.Parse(config.ProxyURL)
		if err != nil {
			log.Printf("代理URL解析失败: %v", err)
			return client
		}
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
	}
	
	return client
}
