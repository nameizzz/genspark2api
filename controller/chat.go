package controller

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"genspark2api/common"
	"genspark2api/common/config"
	logger "genspark2api/common/loggger"
	"genspark2api/model"
	"github.com/deanxv/CycleTLS/cycletls"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL          = "https://www.genspark.ai"
	apiEndpoint      = baseURL + "/api/copilot/ask"
	deleteEndpoint   = baseURL + "/api/project/delete?project_id=%s"
	uploadEndpoint   = baseURL + "/api/get_upload_personal_image_url"
	chatType         = "COPILOT_MOA_CHAT"
	imageType        = "COPILOT_MOA_IMAGE"
	responseIDFormat = "chatcmpl-%s"
)

type OpenAIChatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type OpenAIChatCompletionRequest struct {
	Messages []OpenAIChatMessage
	Model    string
}

// ChatForOpenAI 处理OpenAI聊天请求
func ChatForOpenAI(c *gin.Context) {
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
	cookie, err := common.RandomElement(config.GSCookies)
	if err != nil {
		logger.Errorf(c.Request.Context(), err.Error())
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: err.Error(),
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	}

	if lo.Contains(common.ImageModelList, openAIReq.Model) {
		responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))

		if len(openAIReq.GetUserContent()) == 0 {
			logger.Errorf(c.Request.Context(), "user content is null")
			c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
				OpenAIError: model.OpenAIError{
					Message: "Invalid request parameters",
					Type:    "request_error",
					Code:    "500",
				},
			})
			return
		}

		jsonData, err := json.Marshal(openAIReq.GetUserContent()[0])
		if err != nil {
			logger.Errorf(c.Request.Context(), err.Error())
			c.JSON(500, gin.H{"error": "Failed to marshal request body"})
			return
		}
		resp, err := ImageProcess(c, cookie, model.OpenAIImagesGenerationRequest{
			Model:  openAIReq.Model,
			Prompt: openAIReq.GetUserContent()[0],
		})

		if err != nil {
			logger.Errorf(c.Request.Context(), err.Error())
			c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
				OpenAIError: model.OpenAIError{
					Message: err.Error(),
					Type:    "request_error",
					Code:    "500",
				},
			})
			return
		} else {
			data := resp.Data
			var content []string
			for _, item := range data {
				content = append(content, fmt.Sprintf("![Image](%s)", item.URL))
			}

			if openAIReq.Stream {
				streamResp := createStreamResponse(responseId, openAIReq.Model, jsonData, model.OpenAIDelta{Content: strings.Join(content, "\n"), Role: "assistant"}, nil)
				err := sendSSEvent(c, streamResp)
				if err != nil {
					logger.Errorf(c.Request.Context(), err.Error())
					c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
						OpenAIError: model.OpenAIError{
							Message: err.Error(),
							Type:    "request_error",
							Code:    "500",
						},
					})
					return
				}
				c.SSEvent("", " [DONE]")
				return
			} else {

				jsonBytes, _ := json.Marshal(openAIReq.Messages)
				promptTokens := common.CountTokens(string(jsonBytes))
				completionTokens := common.CountTokens(strings.Join(content, "\n"))

				finishReason := "stop"
				// 创建并返回 OpenAIChatCompletionResponse 结构
				resp := model.OpenAIChatCompletionResponse{
					ID:      fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405")),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   openAIReq.Model,
					Choices: []model.OpenAIChoice{
						{
							Message: model.OpenAIMessage{
								Role:    "assistant",
								Content: strings.Join(content, "\n"),
							},
							FinishReason: &finishReason,
						},
					},
					Usage: model.OpenAIUsage{
						PromptTokens:     promptTokens,
						CompletionTokens: completionTokens,
						TotalTokens:      promptTokens + completionTokens,
					},
				}
				c.JSON(200, resp)
				return
			}

		}
	}

	requestBody, err := createRequestBody(c, cookie, &openAIReq)

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to marshal request body"})
		return
	}

	client := cycletls.Init()

	if openAIReq.Stream {
		handleStreamRequest(c, client, cookie, jsonData, openAIReq.Model)
	} else {
		handleNonStreamRequest(c, client, cookie, jsonData, openAIReq.Model)
	}

}

func processMessages(c *gin.Context, cookie string, messages []model.OpenAIChatMessage) error {
	client := cycletls.Init()

	for i, message := range messages {
		if contentArray, ok := message.Content.([]interface{}); ok {
			for j, content := range contentArray {
				if contentMap, ok := content.(map[string]interface{}); ok {
					if contentType, ok := contentMap["type"].(string); ok && contentType == "image_url" {
						if imageMap, ok := contentMap["image_url"].(map[string]interface{}); ok {
							if url, ok := imageMap["url"].(string); ok {
								err := processUrl(c, client, cookie, url, imageMap, j, contentArray)
								if err != nil {
									logger.Errorf(c.Request.Context(), fmt.Sprintf("processUrl err  %v\n", err))
									return fmt.Errorf("processUrl err: %v", err)
								}
							}
						}
					}
				}
			}
			messages[i].Content = contentArray
		}
	}
	return nil
}
func processUrl(c *gin.Context, client cycletls.CycleTLS, cookie string, url string, imageMap map[string]interface{}, index int, contentArray []interface{}) error {
	// 判断是否为URL
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// 下载文件
		bytes, err := fetchImageBytes(url)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("fetchImageBytes err  %v\n", err))
			return fmt.Errorf("fetchImageBytes err  %v\n", err)
		}

		err = processBytes(c, client, cookie, bytes, imageMap, index, contentArray)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
			return fmt.Errorf("processBytes err  %v\n", err)
		}
	} else {
		// 尝试解析base64
		var bytes []byte
		var err error

		// 处理可能包含 data:image/ 前缀的base64
		base64Str := url
		if strings.Contains(url, ";base64,") {
			base64Str = strings.Split(url, ";base64,")[1]
		}

		bytes, err = base64.StdEncoding.DecodeString(base64Str)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("base64.StdEncoding.DecodeString err  %v\n", err))
			return fmt.Errorf("base64.StdEncoding.DecodeString err: %v\n", err)
		}

		err = processBytes(c, client, cookie, bytes, imageMap, index, contentArray)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("processBytes err  %v\n", err))
			return fmt.Errorf("processBytes err: %v\n", err)
		}
	}
	return nil
}

func processBytes(c *gin.Context, client cycletls.CycleTLS, cookie string, bytes []byte, imageMap map[string]interface{}, index int, contentArray []interface{}) error {
	// 检查是否为图片类型
	contentType := http.DetectContentType(bytes)
	if strings.HasPrefix(contentType, "image/") {
		// 是图片类型，转换为base64
		base64Data := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(bytes)
		imageMap["url"] = base64Data
	} else {
		response, err := makeGetUploadUrlRequest(client, cookie)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("makeGetUploadUrlRequest err  %v\n", err))
			return fmt.Errorf("makeGetUploadUrlRequest err: %v\n", err)
		}

		var jsonResponse map[string]interface{}
		if err := json.Unmarshal([]byte(response.Body), &jsonResponse); err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("Unmarshal err  %v\n", err))
			return fmt.Errorf("Unmarshal err: %v\n", err)
		}

		uploadImageUrl, ok := jsonResponse["data"].(map[string]interface{})["upload_image_url"].(string)
		privateStorageUrl, ok := jsonResponse["data"].(map[string]interface{})["private_storage_url"].(string)

		if !ok {
			//fmt.Println("Failed to extract upload_image_url")
			return fmt.Errorf("Failed to extract upload_image_url")
		}

		// 发送OPTIONS预检请求
		//_, err = makeOptionsRequest(client, uploadImageUrl)
		//if err != nil {
		//	return
		//}
		// 上传文件
		_, err = makeUploadRequest(client, uploadImageUrl, bytes)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("makeUploadRequest err  %v\n", err))
			return fmt.Errorf("makeUploadRequest err: %v\n", err)
		}
		//fmt.Println(resp)

		// 创建新的 private_file 格式的内容
		privateFile := map[string]interface{}{
			"type": "private_file",
			"private_file": map[string]interface{}{
				"name":                "file", // 你可能需要从原始文件名或其他地方获取
				"type":                contentType,
				"size":                len(bytes),
				"ext":                 strings.Split(contentType, "/")[1], // 简单处理，可能需要更复杂的逻辑
				"private_storage_url": privateStorageUrl,
			},
		}

		// 替换数组中的元素
		contentArray[index] = privateFile
	}
	return nil
}

// 获取文件字节数组的函数
func fetchImageBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http.Get err: %v\n", err)
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

func createRequestBody(c *gin.Context, cookie string, openAIReq *model.OpenAIChatCompletionRequest) (map[string]interface{}, error) {
	// 处理消息中的图像 URL
	err := processMessages(c, cookie, openAIReq.Messages)
	if err != nil {
		fmt.Errorf("processMessages err: %v", err)
		return nil, fmt.Errorf("processMessages err: %v", err)
	}

	// 创建请求体
	return map[string]interface{}{
		"type":                 chatType,
		"current_query_string": "type=chat",
		"messages":             openAIReq.Messages,
		//"user_s_input":         openAIReq.Messages[len(openAIReq.Messages)-1].Content,
		"action_params": map[string]interface{}{},
		"extra_data": map[string]interface{}{
			"models":                 []string{openAIReq.Model},
			"run_with_another_model": false,
			"writingContent":         nil,
		},
	}, nil
}

func createImageRequestBody(c *gin.Context, cookie string, openAIReq *model.OpenAIImagesGenerationRequest) map[string]interface{} {

	if openAIReq.Model == "dall-e-3" {
		openAIReq.Model = "dalle-3"
	}
	// 创建模型配置
	modelConfigs := []map[string]interface{}{
		{
			"model":                   openAIReq.Model,
			"aspect_ratio":            "auto",
			"use_personalized_models": false,
			"fashion_profile_id":      nil,
			"hd":                      false,
			"reflection_enabled":      false,
			"style":                   "auto",
		},
	}

	// 创建消息数组
	messages := []map[string]interface{}{
		{
			"role":    "user",
			"content": openAIReq.Prompt,
		},
	}

	// 创建请求体
	return map[string]interface{}{
		"type":                 "COPILOT_MOA_IMAGE",
		"current_query_string": "type=COPILOT_MOA_IMAGE",
		"messages":             messages,
		"user_s_input":         openAIReq.Prompt,
		"action_params":        map[string]interface{}{},
		"extra_data": map[string]interface{}{
			"model_configs":  modelConfigs,
			"llm_model":      "gpt-4o",
			"imageModelMap":  map[string]interface{}{},
			"writingContent": nil,
		},
	}
}

// createStreamResponse 创建流式响应
func createStreamResponse(responseId, modelName string, jsonData []byte, delta model.OpenAIDelta, finishReason *string) model.OpenAIChatCompletionResponse {
	promptTokens := common.CountTokens(string(jsonData))
	completionTokens := common.CountTokens(delta.Content)
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

// handleStreamResponse 处理流式响应
func handleStreamResponse(c *gin.Context, sseChan <-chan cycletls.SSEResponse, responseId, cookie, model string, jsonData []byte) bool {
	var projectId string

	for response := range sseChan {
		if response.Done {
			break
		}

		data := response.Data
		if data == "" {
			continue
		}

		// 处理 "data: " 前缀
		data = strings.TrimSpace(data)
		if !strings.HasPrefix(data, "data: ") {
			continue
		}
		data = strings.TrimPrefix(data, "data: ")

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			logger.Warnf(c.Request.Context(), "Failed to unmarshal event: %v", err)
			continue
		}

		eventType, ok := event["type"].(string)
		if !ok {
			continue
		}

		switch eventType {
		case "project_start":
			projectId, _ = event["id"].(string)
		case "message_field_delta":
			if err := handleMessageFieldDelta(c, event, responseId, model, jsonData); err != nil {
				logger.Warnf(c.Request.Context(), "handleMessageFieldDelta err: %v", err)
				return false
			}
		case "message_result":
			// 删除临时会话
			if config.AutoDelChat == 1 {
				go func() {
					c := cycletls.Init()
					makeDeleteRequest(c, cookie, projectId)
				}()
			}
			return handleMessageResult(c, responseId, model, jsonData)
		}
	}
	return false
}

// handleMessageFieldDelta 处理消息字段增量
func handleMessageFieldDelta(c *gin.Context, event map[string]interface{}, responseId, modelName string, jsonData []byte) error {
	fieldName, ok := event["field_name"].(string)
	if !ok || fieldName != "session_state.answer" {
		return nil
	}

	delta, ok := event["delta"].(string)

	if !ok {
		return nil
	}

	streamResp := createStreamResponse(responseId, modelName, jsonData, model.OpenAIDelta{Content: delta, Role: "assistant"}, nil)
	return sendSSEvent(c, streamResp)
}

// handleMessageResult 处理消息结果
func handleMessageResult(c *gin.Context, responseId, modelName string, jsonData []byte) bool {
	finishReason := "stop"

	streamResp := createStreamResponse(responseId, modelName, jsonData, model.OpenAIDelta{}, &finishReason)
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

// makeRequest 发送HTTP请求
func makeRequest(client cycletls.CycleTLS, jsonData []byte, cookie string, isStream bool) (cycletls.Response, error) {
	accept := "application/json"
	if isStream {
		accept = "text/event-stream"
	}

	return client.Do(apiEndpoint, cycletls.Options{
		Timeout: 10 * 60 * 60,
		Body:    string(jsonData),
		Method:  "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       accept,
			"Origin":       baseURL,
			"Referer":      baseURL + "/",
			"Cookie":       cookie,
		},
	}, "POST")
}

// makeRequest 发送HTTP请求
func makeImageRequest(client cycletls.CycleTLS, jsonData []byte, cookie string) (cycletls.Response, error) {
	accept := "*/*"

	return client.Do(apiEndpoint, cycletls.Options{
		Timeout: 10 * 60 * 60,
		Body:    string(jsonData),
		Method:  "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       accept,
			"Origin":       baseURL,
			"Referer":      baseURL + "/",
			"Cookie":       cookie,
		},
	}, "POST")
}

func makeDeleteRequest(client cycletls.CycleTLS, cookie, projectId string) (cycletls.Response, error) {
	accept := "application/json"

	return client.Do(fmt.Sprintf(deleteEndpoint, projectId), cycletls.Options{
		Timeout: 10 * 60 * 60,
		Method:  "GET",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       accept,
			"Origin":       baseURL,
			"Referer":      baseURL + "/",
			"Cookie":       cookie,
		},
	}, "GET")
}

func makeGetUploadUrlRequest(client cycletls.CycleTLS, cookie string) (cycletls.Response, error) {

	accept := "*/*"

	return client.Do(fmt.Sprintf(uploadEndpoint), cycletls.Options{
		Timeout: 10 * 60 * 60,
		Method:  "GET",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       accept,
			"Origin":       baseURL,
			"Referer":      baseURL + "/",
			"Cookie":       cookie,
		},
	}, "GET")
}

func makeOptionsRequest(client cycletls.CycleTLS, uploadUrl string) (cycletls.Response, error) {
	return client.Do(uploadUrl, cycletls.Options{
		Method: "OPTIONS",
		Headers: map[string]string{
			"Accept":                         "*/*",
			"Access-Control-Request-Headers": "x-ms-blob-type",
			"Access-Control-Request-Method":  "PUT",
			"Origin":                         "https://www.genspark.ai",
			"Sec-Fetch-Dest":                 "empty",
			"Sec-Fetch-Mode":                 "cors",
			"Sec-Fetch-Site":                 "cross-site",
		},
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}, "OPTIONS")
}

func makeUploadRequest(client cycletls.CycleTLS, uploadUrl string, fileBytes []byte) (cycletls.Response, error) {
	return client.Do(uploadUrl, cycletls.Options{
		Timeout: 10 * 60 * 60,
		Method:  "PUT",
		Body:    string(fileBytes),
		Headers: map[string]string{
			"Accept":         "*/*",
			"x-ms-blob-type": "BlockBlob",
			"Content-Type":   "application/octet-stream",
			"Content-Length": fmt.Sprintf("%d", len(fileBytes)),
			"Origin":         "https://www.genspark.ai",
			"Sec-Fetch-Dest": "empty",
			"Sec-Fetch-Mode": "cors",
			"Sec-Fetch-Site": "cross-site",
		},
		Ja3:       "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513,29-23-24,0",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}, "PUT")
}

// handleStreamRequest 处理流式请求
func handleStreamRequest(c *gin.Context, client cycletls.CycleTLS, cookie string, jsonData []byte, model string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))

	c.Stream(func(w io.Writer) bool {
		sseChan, err := makeStreamRequest(c, client, jsonData, cookie)
		if err != nil {
			logger.Errorf(c.Request.Context(), "makeStreamRequest err: %v", err)
			return false
		}

		return handleStreamResponse(c, sseChan, responseId, cookie, model, jsonData)
	})
}

func makeStreamRequest(c *gin.Context, client cycletls.CycleTLS, jsonData []byte, cookie string) (<-chan cycletls.SSEResponse, error) {
	options := cycletls.Options{
		Timeout: 10 * 60 * 60,
		Body:    string(jsonData),
		Method:  "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "text/event-stream",
			"Origin":       baseURL,
			"Referer":      baseURL + "/",
			"Cookie":       cookie,
		},
	}

	sseChan, err := client.DoSSE(apiEndpoint, options, "POST")
	if err != nil {
		logger.Errorf(c, "Failed to make stream request: %v", err)
		return nil, fmt.Errorf("Failed to make stream request: %v", err)
	}
	return sseChan, nil
}

// handleNonStreamRequest 处理非流式请求
func handleNonStreamRequest(c *gin.Context, client cycletls.CycleTLS, cookie string, jsonData []byte, modelName string) {
	response, err := makeRequest(client, jsonData, cookie, false)
	if err != nil {
		logger.Errorf(c.Request.Context(), "makeRequest err: %v", err)
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	reader := strings.NewReader(response.Body)
	scanner := bufio.NewScanner(reader)

	var content string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var parsedResponse struct {
				Type      string `json:"type"`
				FieldName string `json:"field_name"`
				Content   string `json:"content"`
			}
			if err := json.Unmarshal([]byte(data), &parsedResponse); err != nil {
				logger.Warnf(c.Request.Context(), "Failed to unmarshal response: %v", err)
				continue
			}
			if parsedResponse.Type == "message_result" {
				content = parsedResponse.Content
				break
			}
		}
	}

	if content == "" {
		c.JSON(500, gin.H{"error": "No valid response content"})
		return
	}

	promptTokens := common.CountTokens(string(jsonData))
	completionTokens := common.CountTokens(content)

	finishReason := "stop"
	// 创建并返回 OpenAIChatCompletionResponse 结构
	resp := model.OpenAIChatCompletionResponse{
		ID:      fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405")),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []model.OpenAIChoice{
			{
				Message: model.OpenAIMessage{
					Role:    "assistant",
					Content: content,
				},
				FinishReason: &finishReason,
			},
		},
		Usage: model.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}

	c.JSON(200, resp)
}

func OpenaiModels(c *gin.Context) {
	var modelsResp []string

	modelsResp = common.DefaultOpenaiModelList

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

func ImagesForOpenAI(c *gin.Context) {
	var openAIReq model.OpenAIImagesGenerationRequest
	if err := c.BindJSON(&openAIReq); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	cookie, err := common.RandomElement(config.GSCookies)
	if err != nil {
		logger.Errorf(c.Request.Context(), err.Error())
		return
	}

	resp, err := ImageProcess(c, cookie, openAIReq)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("ImageProcess err  %v\n", err))
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: err.Error(),
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	} else {
		c.JSON(200, resp)
	}

}

func ImageProcess(c *gin.Context, cookie string, openAIReq model.OpenAIImagesGenerationRequest) (*model.OpenAIImagesGenerationResponse, error) {
	requestBody := createImageRequestBody(c, cookie, &openAIReq)
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		//c.JSON(500, gin.H{"error": "Failed to marshal request body"})
		return nil, err
	}

	client := cycletls.Init()

	response, err := makeImageRequest(client, jsonData, cookie)

	if err != nil {
		return nil, err
	} else {
		// 解析响应获取task_ids
		projectId, taskIDs := extractTaskIDs(response.Body)
		if len(taskIDs) == 0 {
			return nil, fmt.Errorf("no task IDs found")
		}

		// 获取所有图片URL
		imageURLs := pollTaskStatus(c, client, taskIDs, cookie)

		// 创建响应对象
		response := &model.OpenAIImagesGenerationResponse{
			Created: time.Now().Unix(),
			Data:    make([]*model.OpenAIImagesGenerationDataResponse, 0, len(imageURLs)),
		}

		// 遍历 imageURLs 组装数据
		for _, url := range imageURLs {
			data := &model.OpenAIImagesGenerationDataResponse{
				URL:           url,
				RevisedPrompt: openAIReq.Prompt,
			}

			if openAIReq.ResponseFormat == "b64_json" {
				base64Str, err := getBase64ByUrl(data.URL)
				if err != nil {
					logger.Errorf(c.Request.Context(), fmt.Sprintf("getBase64ByUrl err  %v\n", err))
					return nil, fmt.Errorf("getBase64ByUrl err: %v\n", err)
				}
				data.B64Json = "data:image/webp;base64," + base64Str
			}
			response.Data = append(response.Data, data)
		}

		// 删除临时会话
		if config.AutoDelChat == 1 {
			go func() {
				c := cycletls.Init()
				makeDeleteRequest(c, cookie, projectId)
			}()
		}

		//c.JSON(200, response)
		return response, nil
	}
}

func extractTaskIDs(responseBody string) (string, []string) {
	var taskIDs []string
	var projectId string

	// 分行处理响应
	lines := strings.Split(responseBody, "\n")
	for _, line := range lines {

		// 找到包含project_id的行
		if strings.Contains(line, "project_start") {
			// 去掉"data: "前缀
			jsonStr := strings.TrimPrefix(line, "data: ")

			// 解析JSON
			var jsonResp struct {
				ProjectID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(jsonStr), &jsonResp); err != nil {
				continue
			}

			// 保存project_id
			projectId = jsonResp.ProjectID
		}

		// 找到包含task_id的行
		if strings.Contains(line, "task_id") {
			// 去掉"data: "前缀
			jsonStr := strings.TrimPrefix(line, "data: ")

			// 解析外层JSON
			var outerJSON struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(jsonStr), &outerJSON); err != nil {
				continue
			}

			// 解析内层JSON (content字段)
			var innerJSON struct {
				GeneratedImages []struct {
					TaskID string `json:"task_id"`
				} `json:"generated_images"`
			}
			if err := json.Unmarshal([]byte(outerJSON.Content), &innerJSON); err != nil {
				continue
			}

			// 提取所有task_id
			for _, img := range innerJSON.GeneratedImages {
				if img.TaskID != "" {
					taskIDs = append(taskIDs, img.TaskID)
				}
			}
		}
	}
	return projectId, taskIDs
}

func pollTaskStatus(c *gin.Context, client cycletls.CycleTLS, taskIDs []string, cookie string) []string {
	var imageURLs []string

	for _, taskID := range taskIDs {
		for {
			// 构建请求URL
			url := fmt.Sprintf("https://www.genspark.ai/api/spark/image_generation_task_status?task_id=%s", taskID)

			// 发送请求
			response, err := client.Do(url, cycletls.Options{
				Timeout: 10 * 60 * 60,
				Method:  "GET",
				Headers: map[string]string{
					"Cookie": cookie,
				},
			}, "GET")

			if err != nil {
				continue
			}

			var result struct {
				Data struct {
					ImageURLsNowatermark []string `json:"image_urls_nowatermark"`
					Status               string   `json:"status"`
				}
			}

			if err := json.Unmarshal([]byte(response.Body), &result); err != nil {
				continue
			}

			// 如果状态成功且有图片URL
			if result.Data.Status == "SUCCESS" && len(result.Data.ImageURLsNowatermark) > 0 {
				imageURLs = append(imageURLs, result.Data.ImageURLsNowatermark...)
				break
			}

			// 等待1秒后重试
			time.Sleep(500 * time.Millisecond)
		}
	}

	return imageURLs
}

func getBase64ByUrl(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received non-200 status code: %d", resp.StatusCode)
	}

	imgData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read image data: %w", err)
	}

	// Encode the image data to Base64
	base64Str := base64.StdEncoding.EncodeToString(imgData)
	return base64Str, nil
}
