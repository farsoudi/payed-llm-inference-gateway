package ollama

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Client struct {
	BaseURL     string
	Model       string
	HTTPClient  *http.Client
	MaxBodyLine int
}

func (c *Client) PrepareNative(body []byte, stream bool, maxTokens int) ([]byte, error) {
	request, err := c.prepare(body)
	if err != nil {
		return nil, err
	}
	request["stream"] = stream
	options, _ := request["options"].(map[string]any)
	if options == nil {
		options = make(map[string]any)
		request["options"] = options
	}
	if current, ok := options["num_predict"].(float64); !ok || current < 1 || current > float64(maxTokens) {
		options["num_predict"] = maxTokens
	}
	return json.Marshal(request)
}

func (c *Client) PrepareModel(body []byte) ([]byte, error) {
	request, err := c.prepare(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func (c *Client) PrepareCompatible(body []byte, endpoint string, maxTokens int) ([]byte, error) {
	request, err := c.prepare(body)
	if err != nil {
		return nil, err
	}
	maxField := "max_tokens"
	if endpoint == "/v1/responses" {
		maxField = "max_output_tokens"
	}
	// Only the endpoint's own field (or its chat alias) defines the limit.
	// Unrelated max_* fields cannot raise it, and an over-cap value is lowered.
	limit := maxTokens
	if current, ok := request[maxField].(float64); ok && current >= 1 && current <= float64(maxTokens) {
		limit = int(current)
	} else if endpoint == "/v1/chat/completions" {
		if current, ok := request["max_completion_tokens"].(float64); ok && current >= 1 && current <= float64(maxTokens) {
			limit = int(current)
		}
	}
	// Leave exactly one limit field so reservation reads the enforced value.
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if field != maxField {
			delete(request, field)
		}
	}
	request[maxField] = limit
	if endpoint == "/v1/chat/completions" || endpoint == "/v1/completions" {
		if stream, _ := request["stream"].(bool); stream {
			options, _ := request["stream_options"].(map[string]any)
			if options == nil {
				options = make(map[string]any)
				request["stream_options"] = options
			}
			options["include_usage"] = true
		}
	}
	return json.Marshal(request)
}

func OutputLimit(body []byte, fallback int) int {
	request, err := requestObject(body)
	if err != nil {
		return fallback
	}
	if options, ok := request["options"].(map[string]any); ok {
		if value, ok := options["num_predict"].(float64); ok && value > 0 {
			return int(value)
		}
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value, ok := request[field].(float64); ok && value > 0 {
			return int(value)
		}
	}
	return fallback
}

// prepare decodes the request object and pins the configured model.
func (c *Client) prepare(body []byte) (map[string]any, error) {
	request, err := requestObject(body)
	if err != nil {
		return nil, err
	}
	request["model"] = c.Model
	return request, nil
}

func requestObject(body []byte) (map[string]any, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	return request, nil
}
