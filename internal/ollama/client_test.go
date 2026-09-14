package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreparePinsModelAndCeiling(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.Prepare([]byte(`{"model":"client-model","prompt":"hi","options":{"num_predict":999}}`), true, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"model":"configured"`) || !strings.Contains(got, `"num_predict":100`) || !strings.Contains(got, `"stream":true`) {
		t.Fatalf("prepared body: %s", got)
	}
}

func TestStreamDecodesFinalUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"response":"hi","done":false}` + "\n"))
		_, _ = w.Write([]byte(`{"response":"","done":true,"prompt_eval_count":3,"eval_count":4}` + "\n"))
	}))
	defer server.Close()
	c := Client{BaseURL: server.URL}
	result, err := c.Stream(context.Background(), "/api/generate", []byte(`{}`), func(_ []byte, _ Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.PromptEvalCount != 3 || result.Usage.EvalCount != 4 {
		t.Fatalf("usage: %+v", result.Usage)
	}
}
