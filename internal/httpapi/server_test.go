package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/farsoudi/payed-llm-inference/internal/config"
	"github.com/farsoudi/payed-llm-inference/internal/domain"
	"github.com/farsoudi/payed-llm-inference/internal/ledger"
)

type testStore struct {
	user  domain.User
	debit domain.Debit
}

func (s *testStore) CreateUser(context.Context, string, string, int, int) (domain.User, error) {
	return s.user, nil
}
func (s *testStore) GetUser(context.Context, string) (domain.User, error) { return s.user, nil }
func (s *testStore) ListUsers(context.Context) ([]domain.User, error) {
	return []domain.User{s.user}, nil
}
func (s *testStore) DeleteUser(context.Context, string) error          { return nil }
func (s *testStore) SetLimits(context.Context, string, int, int) error { return nil }
func (s *testStore) CreditTopUp(context.Context, domain.TopUp) (int64, bool, error) {
	return s.user.BalanceMicroUSDC, true, nil
}
func (s *testStore) Debit(_ context.Context, debit domain.Debit) (int64, error) {
	s.debit = debit
	return s.user.BalanceMicroUSDC - debit.CostMicroUSDC, nil
}
func (s *testStore) Close() {}

func testServer(upstream string, store *testStore) *Server {
	cfg := config.Defaults()
	cfg.OllamaURL = upstream
	cfg.OllamaModel = "configured-model"
	cfg.MaxBodyBytes = 1 << 20
	cfg.SafetyMaxTokens = 128
	return New(cfg, store)
}

func TestGatewayOwnedPathsRejectOtherMethods(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("gateway-owned path reached Ollama")
	}))
	defer upstream.Close()
	server := testServer(upstream.URL, &testStore{}).Handler()

	for _, test := range []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/healthz", http.MethodGet},
		{http.MethodPost, "/v1/users/me", http.MethodGet},
		{http.MethodGet, "/v1/topups", http.MethodPost},
	} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != test.allow {
			t.Errorf("%s %s: status %d, Allow %q", test.method, test.path, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
}

func TestProxyForwardsUnlistedOllamaAPIs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" || r.URL.RawQuery != "verbose=true" {
			t.Fatalf("upstream request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		for _, header := range []string{"Authorization", "X-API-Key", "X-PAYMENT", "X-Forwarded-For"} {
			if got := r.Header.Get(header); got != "" {
				t.Errorf("gateway header %s leaked upstream: %q", header, got)
			}
		}
		if r.Header.Get("X-Client-Test") != "preserved" {
			t.Error("ordinary client header was not preserved")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 1000, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/tags?verbose=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("X-PAYMENT", "secret-payment")
	req.Header.Set("X-Client-Test", "preserved")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(data) != `{"models":[]}` {
		t.Fatalf("response: %d %s", resp.StatusCode, data)
	}
}

func TestOpenAIStreamRemainsSSEAndBillsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path: %s", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "configured-model" || request["max_tokens"] != float64(128) || request["stream"] != true {
			t.Errorf("prepared OpenAI request: %#v", request)
		}
		streamOptions, _ := request["stream_options"].(map[string]any)
		if streamOptions["include_usage"] != true {
			t.Errorf("stream usage was not requested: %#v", request)
		}
		if r.Header.Get("X-Client-Test") != "preserved" || r.Header.Get("Authorization") != "" {
			t.Errorf("upstream headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Upstream-Test", "preserved")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-1\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 1000, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	body := strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Test", "preserved")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("response headers: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Upstream-Test") != "preserved" {
		t.Fatalf("upstream response header was not preserved: %#v", resp.Header)
	}
	if !strings.Contains(string(data), "data: [DONE]\n\n") || strings.Contains(string(data), "application/x-ndjson") {
		t.Fatalf("not an SSE response: %s", data)
	}
	if store.debit.CompletionTokens != 1 || store.debit.PromptTokens != 2 || store.debit.CostMicroUSDC != 5 {
		t.Fatalf("debit: %+v", store.debit)
	}
}

func TestMeteredUpstreamErrorIsPreserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ollama-Error", "true")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"message":"unsupported option"}}`)
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 1000, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("X-Ollama-Error") != "true" || string(data) != `{"error":{"message":"unsupported option"}}` {
		t.Fatalf("response: %d %#v %s", resp.StatusCode, resp.Header, data)
	}
	if store.debit.KeyHash != "" {
		t.Fatalf("upstream error was billed: %+v", store.debit)
	}
}

func TestInferenceReservesRequestedMaximumBeforeCallingOllama(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 5, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true,"max_tokens":2}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || called {
		t.Fatalf("reservation admission: status=%d upstream_called=%t", resp.StatusCode, called)
	}
}

func TestMeteredRedirectIsNotFollowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Location", "/redirect-target")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		t.Fatalf("gateway followed redirect to %s", r.URL.Path)
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 1000, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/redirect-target" {
		t.Fatalf("redirect response: %d %#v", resp.StatusCode, resp.Header)
	}
}

func TestLegacyEmbeddingUsesGenericProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Fatalf("upstream path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"embedding":[0.1]}`)
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 0, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/embeddings", strings.NewReader(`{"model":"client","prompt":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy embedding status: %d", resp.StatusCode)
	}
	if store.debit.KeyHash != "" {
		t.Fatalf("legacy embedding without usage was billed: %+v", store.debit)
	}
}

func TestEmbeddingPinsOnlyModelAndAllowsLargeResponse(t *testing.T) {
	padding := strings.Repeat("x", (1<<20)+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "configured-model" || request["max_tokens"] != nil {
			t.Errorf("embedding request: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"padding":"`+padding+`","usage":{"prompt_tokens":2}}`)
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 1000, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/embeddings", strings.NewReader(`{"model":"client","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(data) <= 1<<20 {
		t.Fatalf("large embedding response: %d %d bytes", resp.StatusCode, len(data))
	}
	if store.debit.PromptTokens != 2 || store.debit.CostMicroUSDC != 10 {
		t.Fatalf("embedding debit: %+v", store.debit)
	}
}

func TestProxyKeepsManagementStreamsTransparent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/pull" {
			t.Fatalf("upstream request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"status":"pulling manifest"}`+"\n"+`{"status":"success"}`+"\n")
	}))
	defer upstream.Close()

	store := &testStore{user: domain.User{BalanceMicroUSDC: 0, RateLimitPerMin: 60, ConcurrencyLimit: 1}}
	server := httptest.NewServer(testServer(upstream.URL, store).Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/pull", strings.NewReader(`{"model":"new-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(data) != "{\"status\":\"pulling manifest\"}\n{\"status\":\"success\"}\n" {
		t.Fatalf("response: %d %s", resp.StatusCode, data)
	}
}

var _ ledger.Store = (*testStore)(nil)
