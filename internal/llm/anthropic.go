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

// AnthropicClient se comunica con la Anthropic Messages API.
type AnthropicClient struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

// NewAnthropicClient construye un cliente. baseURL puede estar vacío para
// usar el endpoint público por defecto de la API.
func NewAnthropicClient(apiKey, model, baseURL string) *AnthropicClient {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &AnthropicClient{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// anthropicCacheControl marca un bloque de contenido como punto de corte
// para el prompt cache de Anthropic: todo lo que precede al bloque marcado
// (incluido él) se cachea, así que turnos sucesivos que reenvían el mismo
// prefijo no pagan de nuevo el procesamiento de esos tokens.
type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// anthropicThinking activa el razonamiento extendido de Claude: el modelo
// expone un bloque "thinking" aparte, antes de su respuesta final, hasta
// gastar como máximo BudgetTokens en él.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	System    []anthropicContentBlock `json:"system,omitempty"`
	Messages  []anthropicMessage      `json:"messages"`
	Stream    bool                    `json:"stream,omitempty"`
	Thinking  *anthropicThinking      `json:"thinking,omitempty"`
}

// ephemeralCache es el único tipo de cache_control que soporta la API hoy.
var ephemeralCache = &anthropicCacheControl{Type: "ephemeral"}

// baseMaxTokens es el tope de tokens para la respuesta final (sin contar el
// presupuesto de thinking, que se suma aparte cuando está activo).
const baseMaxTokens = 4096

// thinkingBudgetTokens es el tope de tokens que Claude puede gastar
// razonando antes de responder, cuando el razonamiento extendido está
// activo. max_tokens debe ser mayor que este valor, así que se suma a
// baseMaxTokens en vez de competir por el mismo presupuesto.
const thinkingBudgetTokens = 3000

// buildRequest arma el cuerpo de la petición a partir del historial,
// agregando dos puntos de corte de prompt cache: uno al final del system
// prompt (idéntico en cada turno de una misma tarea) y otro en el último
// mensaje (cachea el historial completo hasta ahí). Sin esto, el agente
// CodeAct -- que reenvía el system prompt y toda la conversación acumulada
// en cada paso de su propio bucle, y entre llamadas sucesivas de la UI web
// -- le hace reprocesar desde cero esos mismos tokens en cada turno, lo
// cual se nota cada vez más conforme crece la conversación.
//
// thinking activa el razonamiento extendido; solo tiene sentido pedirlo
// cuando alguien va a mostrar esos tokens (la UI web en modo streaming), así
// que se deja como parámetro explícito en vez de activarlo siempre.
func buildRequest(model string, messages []Message, stream, thinking bool) anthropicRequest {
	req := anthropicRequest{
		Model:     model,
		MaxTokens: baseMaxTokens,
		Stream:    stream,
	}
	if thinking {
		req.MaxTokens += thinkingBudgetTokens
		req.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: thinkingBudgetTokens}
	}
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			req.System = append(req.System, anthropicContentBlock{Type: "text", Text: m.Content})
		case RoleUser:
			req.Messages = append(req.Messages, anthropicMessage{Role: "user", Content: []anthropicContentBlock{{Type: "text", Text: m.Content}}})
		case RoleAssistant:
			req.Messages = append(req.Messages, anthropicMessage{Role: "assistant", Content: []anthropicContentBlock{{Type: "text", Text: m.Content}}})
		}
	}
	if n := len(req.System); n > 0 {
		req.System[n-1].CacheControl = ephemeralCache
	}
	if n := len(req.Messages); n > 0 {
		lastContent := req.Messages[n-1].Content
		lastContent[len(lastContent)-1].CacheControl = ephemeralCache
	}
	return req
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *AnthropicClient) Complete(ctx context.Context, messages []Message) (string, error) {
	req := buildRequest(c.Model, messages, false, false)

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling anthropic: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decoding response (status %d): %s", resp.StatusCode, string(respBody))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic api error: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic api status %d: %s", resp.StatusCode, string(respBody))
	}

	var out string
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out += block.Text
		}
	}
	return out, nil
}

// CompleteStreaming llama a la Messages API de Anthropic en modo streaming
// (SSE), entrega cada fragmento de la respuesta final a onToken y --si
// onThinking no es nil-- pide razonamiento extendido y le entrega cada
// fragmento de ese razonamiento a onThinking, conforme van llegando.
func (c *AnthropicClient) CompleteStreaming(ctx context.Context, messages []Message, onToken func(string), onThinking func(string)) (string, error) {
	req := buildRequest(c.Model, messages, true, onThinking != nil)

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	// Sin timeout global; el contexto del llamador controla la cancelación.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling anthropic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		var parsed anthropicResponse
		if json.Unmarshal(errBody, &parsed) == nil && parsed.Error != nil {
			return "", fmt.Errorf("anthropic api error: %s", parsed.Error.Message)
		}
		return "", fmt.Errorf("anthropic api status %d: %s", resp.StatusCode, string(errBody))
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var ev struct {
			Type  string `json:"type"`
			Delta *struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Type != "content_block_delta" || ev.Delta == nil {
			continue
		}
		switch ev.Delta.Type {
		case "text_delta":
			onToken(ev.Delta.Text)
			full.WriteString(ev.Delta.Text)
		case "thinking_delta":
			if onThinking != nil {
				onThinking(ev.Delta.Thinking)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), fmt.Errorf("reading stream: %w", err)
	}
	return full.String(), nil
}
