package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type Client struct {
	BaseURL     string
	Model       string
	HTTPClient  *http.Client
	MaxBodyLine int
}

type Event struct {
	Text            string
	Done            bool
	PromptEvalCount int
	EvalCount       int
}

type Result struct {
	Usage Event
	Done  bool
}

func (c *Client) PrepareNative(body []byte, stream bool, maxTokens int) ([]byte, error) {
	request, err := requestObject(body)
	if err != nil {
		return nil, err
	}
	request["model"] = c.Model
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
	request, err := requestObject(body)
	if err != nil {
		return nil, err
	}
	request["model"] = c.Model
	return json.Marshal(request)
}

func (c *Client) PrepareCompatible(body []byte, endpoint string, maxTokens int) ([]byte, error) {
	request, err := requestObject(body)
	if err != nil {
		return nil, err
	}
	request["model"] = c.Model
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

// Proxy forwards the complete Ollama API. Metered inference uses Do so the
// gateway can observe usage; all other routes remain byte-transparent here.
func (c *Client) Proxy(errorHandler func(http.ResponseWriter, *http.Request, error)) http.Handler {
	target, err := c.origin()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { errorHandler(w, r, err) })
	}
	client := c.httpClient()
	return &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			stripGatewayHeaders(proxyRequest.Out.Header)
		},
		Transport:     client.Transport,
		FlushInterval: -1,
		ErrorHandler:  errorHandler,
	}
}

// Do preserves client headers for metered routes while replacing the prepared
// JSON body and removing credentials that belong only to the gateway.
func (c *Client) Do(ctx context.Context, source *http.Request, endpoint string, body []byte) (*http.Response, error) {
	target, err := c.origin()
	if err != nil {
		return nil, err
	}
	upstream := source.Clone(ctx)
	upstream.URL = target.ResolveReference(&url.URL{Path: endpoint, RawQuery: source.URL.RawQuery})
	upstream.Host = ""
	upstream.RequestURI = ""
	upstream.Body = io.NopCloser(bytes.NewReader(body))
	upstream.ContentLength = int64(len(body))
	upstream.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	stripGatewayHeaders(upstream.Header)
	removeHopByHopHeaders(upstream.Header)
	upstream.Header.Del("Content-Length")
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept-Encoding", "identity")
	client := *c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(upstream)
	if err != nil {
		return nil, fmt.Errorf("call Ollama: %w", err)
	}
	return resp, nil
}

func (c *Client) origin() (*url.URL, error) {
	target, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid Ollama URL")
	}
	return target, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func stripGatewayHeaders(header http.Header) {
	for _, name := range []string{
		"Authorization", "X-API-Key", "X-PAYMENT", "X-PAYMENT-RESPONSE",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	} {
		header.Del(name)
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, name := range strings.Split(header.Get("Connection"), ",") {
		header.Del(strings.TrimSpace(name))
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

// StreamNDJSON preserves every byte while parsing a trimmed copy of each line.
func (c *Client) StreamNDJSON(reader io.Reader, onEvent func([]byte, Event) error) (Result, error) {
	buffered := bufio.NewReader(reader)
	limit := c.bodyLineLimit()
	var result Result
	for {
		raw, err := readLine(buffered, limit)
		if len(raw) > 0 {
			event := EventFromJSON(bytes.TrimSpace(raw))
			mergeUsage(&result.Usage, event)
			if event.Done {
				result.Done = true
			}
			if callbackErr := onEvent(raw, event); callbackErr != nil {
				return result, callbackErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("read Ollama stream: %w", err)
		}
	}
	if !result.Done {
		return result, fmt.Errorf("Ollama stream ended without a final usage event")
	}
	return result, nil
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return nil, fmt.Errorf("stream line exceeds %d bytes", limit)
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// StreamSSE preserves complete SSE frames while observing their data payloads.
func (c *Client) StreamSSE(reader io.Reader, onEvent func([]byte, Event) error) (Result, error) {
	buffered := bufio.NewReader(reader)
	limit := c.bodyLineLimit()
	var result Result
	for {
		frame, data, err := readSSEFrame(buffered, limit)
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("read Ollama SSE stream: %w", err)
		}
		event := EventFromJSON(data)
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			event.Done = true
		}
		mergeUsage(&result.Usage, event)
		if event.Done {
			result.Done = true
		}
		if callbackErr := onEvent(frame, event); callbackErr != nil {
			return result, callbackErr
		}
	}
	if !result.Done {
		return result, fmt.Errorf("Ollama SSE stream ended without a terminal event")
	}
	return result, nil
}

func (c *Client) bodyLineLimit() int {
	if c.MaxBodyLine > 0 {
		return c.MaxBodyLine
	}
	return 2 << 20
}

func readSSEFrame(reader *bufio.Reader, limit int) ([]byte, []byte, error) {
	var frame bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if frame.Len()+len(line) > limit {
			return nil, nil, fmt.Errorf("SSE frame exceeds %d bytes", limit)
		}
		frame.Write(line)
		if err != nil {
			if err == io.EOF && frame.Len() == 0 {
				return nil, nil, io.EOF
			}
			if err != io.EOF {
				return nil, nil, err
			}
			break
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	var data []byte
	for _, line := range bytes.Split(frame.Bytes(), []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))...)
	}
	return frame.Bytes(), data, nil
}

// EventFromJSON extracts only documented text and usage fields. Unknown
// protocol fields remain opaque and are forwarded unchanged by the caller.
func EventFromJSON(data []byte) Event {
	var payload struct {
		Type            string          `json:"type"`
		Done            bool            `json:"done"`
		Response        json.RawMessage `json:"response"`
		PromptEvalCount int             `json:"prompt_eval_count"`
		EvalCount       int             `json:"eval_count"`
		Text            string          `json:"text"`
		Delta           json.RawMessage `json:"delta"`
		Message         struct {
			Content optionalText `json:"content"`
			Usage   usage        `json:"usage"`
		} `json:"message"`
		Choices []struct {
			Text    string  `json:"text"`
			Delta   content `json:"delta"`
			Message content `json:"message"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return Event{}
	}
	var responseText string
	var responseObject struct {
		Usage usage `json:"usage"`
	}
	if len(payload.Response) > 0 {
		_ = json.Unmarshal(payload.Response, &responseText)
		_ = json.Unmarshal(payload.Response, &responseObject)
	}
	var deltaText string
	var deltaObject struct {
		Content optionalText `json:"content"`
		Text    optionalText `json:"text"`
	}
	if len(payload.Delta) > 0 {
		_ = json.Unmarshal(payload.Delta, &deltaText)
		_ = json.Unmarshal(payload.Delta, &deltaObject)
	}
	event := Event{
		Text:            responseText + payload.Text + deltaText + string(deltaObject.Content) + string(deltaObject.Text) + string(payload.Message.Content),
		Done:            payload.Done || payload.Type == "response.completed" || payload.Type == "response.incomplete" || payload.Type == "response.failed" || payload.Type == "message_stop",
		PromptEvalCount: payload.PromptEvalCount,
		EvalCount:       payload.EvalCount,
	}
	for _, choice := range payload.Choices {
		event.Text += choice.Text + string(choice.Delta.Content) + string(choice.Message.Content)
	}
	mergeUsageFields(&event, payload.Usage)
	mergeUsageFields(&event, payload.Message.Usage)
	mergeUsageFields(&event, responseObject.Usage)
	return event
}

type content struct {
	Content optionalText `json:"content"`
}

type optionalText string

func (text *optionalText) UnmarshalJSON(data []byte) error {
	var value string
	if json.Unmarshal(data, &value) == nil {
		*text = optionalText(value)
	}
	return nil
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func mergeUsageFields(event *Event, value usage) {
	if n := max(value.PromptTokens, value.InputTokens); n > event.PromptEvalCount {
		event.PromptEvalCount = n
	}
	if n := max(value.CompletionTokens, value.OutputTokens); n > event.EvalCount {
		event.EvalCount = n
	}
}

func mergeUsage(total *Event, event Event) {
	if event.PromptEvalCount > total.PromptEvalCount {
		total.PromptEvalCount = event.PromptEvalCount
	}
	if event.EvalCount > total.EvalCount {
		total.EvalCount = event.EvalCount
	}
}
