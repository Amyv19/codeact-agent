package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIClient se comunica con cualquier endpoint de chat completions
// compatible con OpenAI. Esto incluye a OpenAI mismo y a runners locales
// como Ollama o LM Studio cuando se apuntan a sus rutas de compatibilidad
// "/v1", que es como este mismo cliente también sirve como backend para
// modelos locales.
type OpenAIClient struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

func NewOpenAIClient(apiKey, model, baseURL string) *OpenAIClient {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIClient{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *OpenAIClient) Complete(ctx context.Context, messages []Message) (string, error) {
	req := openAIRequest{Model: c.Model}
	for _, m := range messages {
		req.Messages = append(req.Messages, openAIMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling openai-compatible endpoint: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decoding response (status %d): %s", resp.StatusCode, string(respBody))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("api error: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, string(respBody))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return parsed.Choices[0].Message.Content, nil
}

// CompleteStreaming llama al endpoint de chat completions en modo streaming
// (SSE) y entrega cada fragmento de texto a onToken conforme llega.
// onThinking nunca se llama: la API de chat completions no expone
// razonamiento extendido por separado del texto final.
func (c *OpenAIClient) CompleteStreaming(ctx context.Context, messages []Message, onToken func(string), onThinking func(string)) (string, error) {
	type streamReq struct {
		Model    string          `json:"model"`
		Messages []openAIMessage `json:"messages"`
		Stream   bool            `json:"stream"`
	}
	req := streamReq{Model: c.Model, Stream: true}
	for _, m := range messages {
		req.Messages = append(req.Messages, openAIMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	streamClient := &http.Client{}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling openai: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		var parsed openAIResponse
		if json.Unmarshal(errBody, &parsed) == nil && parsed.Error != nil {
			return "", fmt.Errorf("api error: %s", parsed.Error.Message)
		}
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, string(errBody))
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			text := chunk.Choices[0].Delta.Content
			if text != "" {
				onToken(text)
				full.WriteString(text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), fmt.Errorf("reading stream: %w", err)
	}
	return full.String(), nil
}
