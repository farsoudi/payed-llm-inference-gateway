package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	BaseURL     string
	Model       string
	HTTPClient  *http.Client
	MaxBodyLine int
}

type Event struct {
	Model           string `json:"model"`
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"`
	Message         struct {
		Content string `json:"content"`
	} `json:"message"`
}

type Result struct {
	Usage   Event
	Stopped bool
	Done    bool
}

var ErrStopped = fmt.Errorf("ollama stream stopped by gateway")

func (c *Client) Prepare(body []byte, forceStream bool, maxTokens int) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	request["model"] = c.Model
	request["stream"] = forceStream
	options, _ := request["options"].(map[string]any)
	if options == nil {
		options = make(map[string]any)
		request["options"] = options
	}
	if current, ok := options["num_predict"].(float64); !ok || current <= 0 || int(current) > maxTokens {
		options["num_predict"] = maxTokens
	}
	return json.Marshal(request)
}

func (c *Client) Stream(ctx context.Context, endpoint string, body []byte, onEvent func(raw []byte, event Event) error) (Result, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create Ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("call Ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("Ollama returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	scanner := bufio.NewScanner(resp.Body)
	limit := c.MaxBodyLine
	if limit <= 0 {
		limit = 2 << 20
	}
	scanner.Buffer(make([]byte, 64<<10), limit)
	var result Result
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return result, fmt.Errorf("decode Ollama stream: %w", err)
		}
		if err := onEvent(raw, event); err != nil {
			if err == ErrStopped {
				result.Stopped = true
				return result, nil
			}
			return result, err
		}
		if event.Done {
			result.Usage = event
			result.Done = true
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read Ollama stream: %w", err)
	}
	if !result.Done {
		return result, fmt.Errorf("Ollama stream ended without a final usage event")
	}
	return result, nil
}
