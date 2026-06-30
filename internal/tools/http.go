package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTP expone una sola capacidad http_get deliberadamente acotada: solo
// GET, tamaño de respuesta limitado, timeout limitado. Suficiente para
// health checks y consultas rápidas a APIs desde un script generado.
type HTTP struct {
	Client     *http.Client
	MaxBodyLen int64
}

func NewHTTP(timeout time.Duration, maxBodyLen int64) *HTTP {
	return &HTTP{Client: &http.Client{Timeout: timeout}, MaxBodyLen: maxBodyLen}
}

type HTTPResult struct {
	Status int
	Body   string
}

func (h *HTTP) Get(url string, headers map[string]string) (HTTPResult, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return HTTPResult{}, fmt.Errorf("only http:// and https:// urls are allowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.Client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HTTPResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		return HTTPResult{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, h.MaxBodyLen))
	if err != nil {
		return HTTPResult{}, err
	}
	return HTTPResult{Status: resp.StatusCode, Body: string(body)}, nil
}
