package controller

import (
	"alexsidebar2api/alexsidebar-api"
	"alexsidebar2api/common"
	"alexsidebar2api/common/config"
	logger "alexsidebar2api/common/loggger"
	"alexsidebar2api/cycletls"
	"alexsidebar2api/model"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	errServerErrMsg  = "Service Unavailable"
	responseIDFormat = "chatcmpl-%s"
)

// ChatForOpenAI @Summary OpenAI对话接口
// @Description OpenAI对话接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param req body model.OpenAIChatCompletionRequest true "OpenAI对话请求"
// @Param Authorization header string true "Authorization API-KEY"
// @Router /v1/chat/completions [post]
func ChatForOpenAI(c *gin.Context) {
	client := cycletls.Init()
	defer safeClose(client)

	var openAIReq model.OpenAIChatCompletionRequest
	if err := c.BindJSON(&openAIReq); err != nil {
		logger.Errorf(c.Request.Context(), err.Error())
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: "Invalid request parameters",
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	}

	openAIReq.RemoveEmptyContentMessages()

	modelInfo, b := common.GetModelInfo(openAIReq.Model)
	if !b {
		c.JSON(http.StatusBadRequest, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: fmt.Sprintf("Model %s not supported", openAIReq.Model),
				Type:    "invalid_request_error",
				Code:    "invalid_model",
			},
		})
		return
	}
	if openAIReq.MaxTokens > modelInfo.MaxTokens {
		c.JSON(http.StatusBadRequest, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: fmt.Sprintf("Max tokens %d exceeds limit %d", openAIReq.MaxTokens, modelInfo.MaxTokens),
				Type:    "invalid_request_error",
				Code:    "invalid_max_tokens",
			},
		})
		return
	}

	if openAIReq.Stream {
		handleStreamRequest(c, client, openAIReq, modelInfo)
	} else {
		handleNonStreamRequest(c, client, openAIReq, modelInfo)
	}
}

func handleNonStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) {
	ctx := c.Request.Context()
	cookieManager := config.NewCookieManager()
	maxRetries := len(cookieManager.Cookies)
	cookie, err := cookieManager.GetRandomCookie()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for attempt := 0; attempt < maxRetries; attempt++ {
		requestBody, err := createRequestBody(c, &openAIReq, modelInfo)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		jsonData, err := json.Marshal(requestBody)
		if err != nil {
			c.JSON(500, gin.H{"error": "Failed to marshal request body"})
			return
		}
		sseChan, err := alexsidebar_api.MakeStreamChatRequest(c, client, jsonData, cookie)
		if err != nil {
			logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		isRateLimit := false
		var delta string
		var assistantMsgContent string
		var shouldContinue bool
		thinkStartType := new(bool)
		thinkEndType := new(bool)
		var lastText string
		var lastCode string
	SSELoop:
		for response := range sseChan {
			data := response.Data
			if data == "" {
				continue
			}

			if response.Done && data != "[DONE]" {
				switch {
				case common.IsUsageLimitExceeded(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie Usage limit exceeded, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					config.RemoveCookie(cookie)
					break SSELoop
				case common.IsChineseChat(data):
					logger.Errorf(ctx, data)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Detected that you are using Chinese for conversation, please use English for conversation."})
					return
				case common.IsNotLogin(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					break SSELoop
				case common.IsRateLimit(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie rate limited, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					config.AddRateLimitCookie(cookie, time.Now().Add(time.Duration(config.RateLimitCookieLockDuration)*time.Second))
					break SSELoop
				}
				logger.Warnf(ctx, response.Data)
				return
			}

			logger.Debug(ctx, strings.TrimSpace(data))

			streamDelta, streamShouldContinue := processNoStreamData(c, data, thinkStartType, thinkEndType, &lastText, &lastCode)
			delta = streamDelta
			shouldContinue = streamShouldContinue
			// 处理事件流数据
			if !shouldContinue {
				promptTokens := model.CountTokenText(string(jsonData), openAIReq.Model)
				completionTokens := model.CountTokenText(assistantMsgContent, openAIReq.Model)
				finishReason := "stop"

				c.JSON(http.StatusOK, model.OpenAIChatCompletionResponse{
					ID:      fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405")),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   openAIReq.Model,
					Choices: []model.OpenAIChoice{{
						Message: model.OpenAIMessage{
							Role:    "assistant",
							Content: assistantMsgContent,
						},
						FinishReason: &finishReason,
					}},
					Usage: model.OpenAIUsage{
						PromptTokens:     promptTokens,
						CompletionTokens: completionTokens,
						TotalTokens:      promptTokens + completionTokens,
					},
				})

				return
			} else {
				assistantMsgContent = assistantMsgContent + delta
			}
		}
		if !isRateLimit {
			return
		}

		// 获取下一个可用的cookie继续尝试
		cookie, err = cookieManager.GetNextCookie()
		if err != nil {
			logger.Errorf(ctx, "No more valid cookies available after attempt %d", attempt+1)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

	}
	logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
	return
}

func createRequestBody(c *gin.Context, openAIReq *model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) (map[string]interface{}, error) {
	client := cycletls.Init()
	defer safeClose(client)

	if openAIReq.MaxTokens <= 1 {
		openAIReq.MaxTokens = 8000
	}

	logger.Debug(c.Request.Context(), fmt.Sprintf("RequestBody: %v", openAIReq))

	requestBody := map[string]interface{}{
		"model":  modelInfo.Model,
		"prompt": "",
		"api_keys": map[string]string{
			"anthropic":  "",
			"perplexity": "",
			"gemini":     "",
			"openai":     "",
		},
		"deps": []interface{}{},
	}

	messages := []map[string]interface{}{}

	isThinkingModel := strings.HasSuffix(openAIReq.Model, "-thinking")

	for i, msg := range openAIReq.Messages {
		// 将角色转换为首字母大写的格式
		formattedRole := capitalizeRole(msg.Role)

		formattedMsg := map[string]interface{}{
			"role":           formattedRole, // 使用转换后的角色
			"id":             i,
			"ts_created":     time.Now().Unix(),
			"web_access":     false,
			"img_urls":       []string{},
			"relevant_files": []string{},
			"docs":           []interface{}{},
			"code_contexts":  []interface{}{},
			"think_first":    false,
		}

		if isThinkingModel && msg.Role == "user" && i == len(openAIReq.Messages)-1 {
			formattedMsg["think_first"] = true
		}

		// Process content
		sections := []map[string]interface{}{}

		switch content := msg.Content.(type) {
		case string:
			sections = append(sections, map[string]interface{}{
				"text": map[string]interface{}{
					"text":        content,
					"is_thinking": false,
				},
			})
		case []interface{}:
			for _, part := range content {
				if contentMap, ok := part.(map[string]interface{}); ok {
					if contentType, exists := contentMap["type"]; exists && contentType == "text" {
						if textContent, hasText := contentMap["text"]; hasText {
							sections = append(sections, map[string]interface{}{
								"text": map[string]interface{}{
									"text":        textContent,
									"is_thinking": false,
								},
							})
						}
					}
				}
			}
		}

		formattedMsg["sections"] = sections
		messages = append(messages, formattedMsg)
	}

	requestBody["messages"] = messages

	return requestBody, nil
}

// 将角色转换为首字母大写的格式
func capitalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	default:
		// 对于未知角色，也尝试首字母大写
		if len(role) > 0 {
			return strings.ToUpper(role[:1]) + role[1:]
		}
		return role
	}
}

// createStreamResponse 创建流式响应
func createStreamResponse(responseId, modelName string, jsonData []byte, delta model.OpenAIDelta, finishReason *string) model.OpenAIChatCompletionResponse {
	promptTokens := model.CountTokenText(string(jsonData), modelName)
	completionTokens := model.CountTokenText(delta.Content, modelName)
	return model.OpenAIChatCompletionResponse{
		ID:      responseId,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []model.OpenAIChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			},
		},
		Usage: model.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// handleDelta 处理消息字段增量
func handleDelta(c *gin.Context, delta string, responseId, modelName string, jsonData []byte) error {
	// 创建基础响应
	createResponse := func(content string) model.OpenAIChatCompletionResponse {
		return createStreamResponse(
			responseId,
			modelName,
			jsonData,
			model.OpenAIDelta{Content: content, Role: "assistant"},
			nil,
		)
	}

	// 发送基础事件
	var err error
	if err = sendSSEvent(c, createResponse(delta)); err != nil {
		return err
	}

	return err
}

// handleMessageResult 处理消息结果
func handleMessageResult(c *gin.Context, responseId, modelName string, jsonData []byte) bool {
	finishReason := "stop"
	var delta string

	promptTokens := 0
	completionTokens := 0

	streamResp := createStreamResponse(responseId, modelName, jsonData, model.OpenAIDelta{Content: delta, Role: "assistant"}, &finishReason)
	streamResp.Usage = model.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	if err := sendSSEvent(c, streamResp); err != nil {
		logger.Warnf(c.Request.Context(), "sendSSEvent err: %v", err)
		return false
	}
	c.SSEvent("", " [DONE]")
	return false
}

// sendSSEvent 发送SSE事件
func sendSSEvent(c *gin.Context, response model.OpenAIChatCompletionResponse) error {
	jsonResp, err := json.Marshal(response)
	if err != nil {
		logger.Errorf(c.Request.Context(), "Failed to marshal response: %v", err)
		return err
	}
	c.SSEvent("", " "+string(jsonResp))
	c.Writer.Flush()
	return nil
}

func handleStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))
	ctx := c.Request.Context()

	cookieManager := config.NewCookieManager()
	maxRetries := len(cookieManager.Cookies)
	cookie, err := cookieManager.GetRandomCookie()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	thinkStartType := new(bool)
	thinkEndType := new(bool)

	// 为每个请求创建独立的状态变量
	var lastText string
	var lastCode string

	c.Stream(func(w io.Writer) bool {
		for attempt := 0; attempt < maxRetries; attempt++ {
			requestBody, err := createRequestBody(c, &openAIReq, modelInfo)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return false
			}

			jsonData, err := json.Marshal(requestBody)
			if err != nil {
				c.JSON(500, gin.H{"error": "Failed to marshal request body"})
				return false
			}
			sseChan, err := alexsidebar_api.MakeStreamChatRequest(c, client, jsonData, cookie)
			if err != nil {
				logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return false
			}

			isRateLimit := false
		SSELoop:
			for response := range sseChan {
				data := response.Data
				if data == "" {
					continue
				}

				if response.Done && data != "[DONE]" {
					switch {
					case common.IsUsageLimitExceeded(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie Usage limit exceeded, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						config.RemoveCookie(cookie)
						break SSELoop
					case common.IsChineseChat(data):
						logger.Errorf(ctx, data)
						c.JSON(http.StatusInternalServerError, gin.H{"error": "Detected that you are using Chinese for conversation, please use English for conversation."})
						return false
					case common.IsNotLogin(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						break SSELoop // 使用 label 跳出 SSE 循环
					case common.IsRateLimit(data):
						isRateLimit = true
						logger.Warnf(ctx, "Cookie rate limited, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
						config.AddRateLimitCookie(cookie, time.Now().Add(time.Duration(config.RateLimitCookieLockDuration)*time.Second))
						break SSELoop
					}
					logger.Warnf(ctx, response.Data)
					return false
				}

				logger.Debug(ctx, strings.TrimSpace(data))

				_, shouldContinue := processStreamData(c, data, responseId, openAIReq.Model, jsonData, thinkStartType, thinkEndType, &lastText, &lastCode)
				// 处理事件流数据

				if !shouldContinue {
					return false
				}
			}

			if !isRateLimit {
				return true
			}

			// 获取下一个可用的cookie继续尝试
			cookie, err = cookieManager.GetNextCookie()
			if err != nil {
				logger.Errorf(ctx, "No more valid cookies available after attempt %d", attempt+1)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return false
			}
		}

		logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
		return false
	})
}

func processStreamData(c *gin.Context, data, responseId, model string, jsonData []byte, thinkStartType, thinkEndType *bool, lastText, lastCode *string) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")
	if data == "[DONE]" {
		handleMessageResult(c, responseId, model, jsonData)
		return "", false
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		logger.Errorf(c.Request.Context(), "Failed to unmarshal event: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return "", false
	}

	sections, ok := event["sections"].([]interface{})
	if !ok || len(sections) == 0 {
		return "", true
	}

	section := sections[len(sections)-1].(map[string]interface{})

	// 检查是否存在thinking状态
	if textObj, ok := section["text"].(map[string]interface{}); ok && textObj != nil {
		isThinking, _ := textObj["is_thinking"].(bool)

		// 处理thinking开始和结束
		if isThinking && !*thinkStartType {
			// 第一次检测到thinking开始
			*thinkStartType = true
			if err := handleDelta(c, "<think>", responseId, model, jsonData); err != nil {
				logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return "", false
			}
		} else if !isThinking && *thinkStartType && !*thinkEndType {
			// thinking结束
			*thinkEndType = true
			if err := handleDelta(c, "</think>", responseId, model, jsonData); err != nil {
				logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return "", false
			}
		}
	}

	if *lastCode != "" && section["text"] != nil && section["code"] == nil {
		if err := handleDelta(c, "\n\n", responseId, model, jsonData); err != nil {
			logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return "", false
		}
		*lastCode = "" // 清空最后的代码
	}

	var textDelta string
	if textObj, ok := section["text"].(map[string]interface{}); ok && textObj != nil {
		if text, ok := textObj["text"].(string); ok && text != "" {
			textDelta = getTextDelta(text, lastText)

			if textDelta != "" {
				if err := handleDelta(c, textDelta, responseId, model, jsonData); err != nil {
					logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return "", false
				}

				text = strings.TrimSuffix(text, "</new_")
				text = strings.TrimSuffix(text, "</new")

				*lastText = text // 更新最后的文本
				return textDelta, true
			}
		}
	}

	if codeObj, ok := section["code"].(map[string]interface{}); ok && codeObj != nil {
		if code, ok := codeObj["code"].(string); ok {
			codeDelta, isFirstCode := getCodeDelta(code, lastCode)

			if codeDelta != "" {
				if isFirstCode {
					codeDelta = "\n\n" + codeDelta
				}

				if err := handleDelta(c, codeDelta, responseId, model, jsonData); err != nil {
					logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return "", false
				}
				*lastCode = code
			} else if *lastCode != "" && code == "" {
				if err := handleDelta(c, "\n\n", responseId, model, jsonData); err != nil {
					logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return "", false
				}
				*lastCode = ""
			}

			return codeDelta, true
		}
	}

	return "", true
}

// 获取文本增量
func getTextDelta(currentText string, lastText *string) string {
	if *lastText == "" {
		return currentText
	}

	currentText = strings.TrimSuffix(currentText, "</new_")
	currentText = strings.TrimSuffix(currentText, "</new")

	if strings.HasPrefix(strings.TrimSpace(currentText), strings.TrimSpace(*lastText)) {
		// 返回增量部分
		if len(*lastText) > len(currentText) {
			currentText = strings.TrimSpace(currentText)
			*lastText = strings.TrimSpace(*lastText)
		}
		return currentText[len(*lastText):]
	}

	return "\n" + currentText
}

// 获取代码增量并判断是否是第一次有代码
func getCodeDelta(currentCode string, lastCode *string) (string, bool) {
	isFirstCode := *lastCode == "" && currentCode != ""

	// 如果没有上一次的代码，返回整个当前代码
	if *lastCode == "" {
		return currentCode, isFirstCode
	}

	// 检查当前代码是否以上一次的代码开头
	if strings.HasPrefix(currentCode, *lastCode) {
		// 返回增量部分
		return currentCode[len(*lastCode):], isFirstCode
	}

	// 如果当前代码与上一次的代码不连续，返回整个当前代码
	return currentCode, isFirstCode
}

func processNoStreamData(c *gin.Context, data string, thinkStartType *bool, thinkEndType *bool, lastText, lastCode *string) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")
	if data == "[DONE]" {
		return "", false
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		logger.Errorf(c.Request.Context(), "Failed to unmarshal event: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return "", false
	}

	sections, ok := event["sections"].([]interface{})
	if !ok || len(sections) == 0 {
		return "", true
	}

	section := sections[len(sections)-1].(map[string]interface{})

	if *lastCode != "" && section["text"] != nil && section["code"] == nil {
		*lastCode = "" // 清空最后的代码
	}

	var textDelta string
	if textObj, ok := section["text"].(map[string]interface{}); ok && textObj != nil {
		if text, ok := textObj["text"].(string); ok && text != "" {
			textDelta = getTextDelta(text, lastText)

			if textDelta != "" {

				text = strings.TrimSuffix(text, "</new_")
				text = strings.TrimSuffix(text, "</new")

				*lastText = text // 更新最后的文本
				// 检查是否存在thinking状态
				if textObj, ok := section["text"].(map[string]interface{}); ok && textObj != nil {
					isThinking, _ := textObj["is_thinking"].(bool)

					// 处理thinking开始和结束
					if isThinking && !*thinkStartType {
						// 第一次检测到thinking开始
						*thinkStartType = true
						textDelta = "<think>\n\n" + textDelta
					} else if !isThinking && *thinkStartType && !*thinkEndType {
						// thinking结束
						*thinkEndType = true
						textDelta = textDelta + "\n\n</think>\n\n"
					}
				}

				return textDelta, true
			}
		}
	}

	if codeObj, ok := section["code"].(map[string]interface{}); ok && codeObj != nil {
		if code, ok := codeObj["code"].(string); ok {
			codeDelta, isFirstCode := getCodeDelta(code, lastCode)

			if codeDelta != "" {
				if isFirstCode {
					codeDelta = "\n\n" + codeDelta
				}

				*lastCode = code
			} else if *lastCode != "" && code == "" {

				*lastCode = ""
			}

			return codeDelta, true
		}
	}

	return "", true

}

// OpenaiModels @Summary OpenAI模型列表接口
// @Description OpenAI模型列表接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param Authorization header string true "Authorization API-KEY"
// @Success 200 {object} common.ResponseResult{data=model.OpenaiModelListResponse} "成功"
// @Router /v1/models [get]
func OpenaiModels(c *gin.Context) {
	var modelsResp []string

	modelsResp = lo.Union(common.GetModelList())

	var openaiModelListResponse model.OpenaiModelListResponse
	var openaiModelResponse []model.OpenaiModelResponse
	openaiModelListResponse.Object = "list"

	for _, modelResp := range modelsResp {
		openaiModelResponse = append(openaiModelResponse, model.OpenaiModelResponse{
			ID:     modelResp,
			Object: "model",
		})
	}
	openaiModelListResponse.Data = openaiModelResponse
	c.JSON(http.StatusOK, openaiModelListResponse)
	return
}

func safeClose(client cycletls.CycleTLS) {
	if client.ReqChan != nil {
		close(client.ReqChan)
	}
	if client.RespChan != nil {
		close(client.RespChan)
	}
}

//
//func processUrl(c *gin.Context, client cycletls.CycleTLS, chatId, cookie string, url string) (string, error) {
//	// 判断是否为URL
//	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
//		// 下载文件
//		bytes, err := fetchImageBytes(url)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("fetchImageBytes err  %v\n", err))
//			return "", fmt.Errorf("fetchImageBytes err  %v\n", err)
//		}
//
//		base64Str := base64.StdEncoding.EncodeToString(bytes)
//
//		finalUrl, err := processBytes(c, client, chatId, cookie, base64Str)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
//			return "", fmt.Errorf("processBytes err  %v\n", err)
//		}
//		return finalUrl, nil
//	} else {
//		finalUrl, err := processBytes(c, client, chatId, cookie, url)
//		if err != nil {
//			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
//			return "", fmt.Errorf("processBytes err  %v\n", err)
//		}
//		return finalUrl, nil
//	}
//}
//
//func fetchImageBytes(url string) ([]byte, error) {
//	resp, err := http.Get(url)
//	if err != nil {
//		return nil, fmt.Errorf("http.Get err: %v\n", err)
//	}
//	defer resp.Body.Close()
//
//	return io.ReadAll(resp.Body)
//}
//
//func processBytes(c *gin.Context, client cycletls.CycleTLS, chatId, cookie string, base64Str string) (string, error) {
//	// 检查类型
//	fileType := common.DetectFileType(base64Str)
//	if !fileType.IsValid {
//		return "", fmt.Errorf("invalid file type %s", fileType.Extension)
//	}
//	signUrl, err := alexsidebar-api.GetSignURL(client, cookie, chatId, fileType.Extension)
//	if err != nil {
//		logger.Errorf(c.Request.Context(), fmt.Sprintf("GetSignURL err  %v\n", err))
//		return "", fmt.Errorf("GetSignURL err: %v\n", err)
//	}
//
//	err = alexsidebar-api.UploadToS3(client, signUrl, base64Str, fileType.MimeType)
//	if err != nil {
//		logger.Errorf(c.Request.Context(), fmt.Sprintf("UploadToS3 err  %v\n", err))
//		return "", err
//	}
//
//	u, err := url.Parse(signUrl)
//	if err != nil {
//		return "", err
//	}
//
//	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path), nil
//}
