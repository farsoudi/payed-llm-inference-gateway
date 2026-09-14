package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/farsoudi/payed-llm-inference/internal/auth"
	"github.com/farsoudi/payed-llm-inference/internal/config"
	"github.com/farsoudi/payed-llm-inference/internal/domain"
	"github.com/farsoudi/payed-llm-inference/internal/guardrails"
	"github.com/farsoudi/payed-llm-inference/internal/ledger"
	"github.com/farsoudi/payed-llm-inference/internal/meter"
	"github.com/farsoudi/payed-llm-inference/internal/money"
	"github.com/farsoudi/payed-llm-inference/internal/ollama"
	"github.com/farsoudi/payed-llm-inference/internal/payment"
)

type Server struct {
	Config   config.Config
	Store    ledger.Store
	Meter    *meter.Meter
	Limits   *guardrails.Controller
	Failures *guardrails.FailureMonitor
	Ollama   *ollama.Client
	Proxy    http.Handler
	Payments *payment.Service
	Logger   *slog.Logger
}

func New(cfg config.Config, store ledger.Store) *Server {
	s := &Server{
		Config:   cfg,
		Store:    store,
		Meter:    meter.New(store, cfg.PricePerTokenMicro),
		Limits:   guardrails.New(),
		Failures: guardrails.NewFailureMonitor(),
		Ollama:   &ollama.Client{BaseURL: cfg.OllamaURL, Model: cfg.OllamaModel, HTTPClient: &http.Client{Timeout: cfg.RequestTimeout}},
		Payments: payment.New(cfg),
		Logger:   slog.Default(),
	}
	s.Proxy = s.Ollama.Proxy(s.proxyError)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/users/me", s.me)
	mux.HandleFunc("POST /v1/topups", s.topup)
	mux.HandleFunc("/healthz", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("/v1/users/me", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("/v1/topups", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("/", s.proxy)
	return requestLogging(mux, s.Logger)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.user(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) topup(w http.ResponseWriter, r *http.Request) {
	_, hash, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		Amount json.RawMessage `json:"amount"`
	}
	if err := decodeBody(w, r, &request, s.Config.MaxBodyBytes); err != nil {
		s.noteFailure(r, "malformed_topup")
		return
	}
	amountText := strings.Trim(string(request.Amount), " \"")
	amount, err := money.ParseUSDC(amountText)
	if err != nil || amount < s.Config.MinTopupMicro {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("amount must be at least %s USDC", money.FormatUSDC(s.Config.MinTopupMicro)))
		return
	}

	protected := s.Payments.Protect(amount, "Fund inference gateway balance", func(settlement payment.Settlement) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			balance, inserted, err := s.Meter.CreditTopUp(r.Context(), domain.TopUp{KeyHash: hash, Amount: amount, Transaction: settlement.Transaction, Payer: settlement.Payer, Network: settlement.Network})
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"credited":     inserted,
				"amount_usdc":  money.FormatUSDC(amount),
				"balance_usdc": money.FormatUSDC(balance),
				"transaction":  settlement.Transaction,
			})
		})
	})
	protected.ServeHTTP(w, r)
}

type inferenceProtocol int

const (
	nativeNDJSON inferenceProtocol = iota
	compatibleSSE
	jsonResponse
)

type inferenceSpec struct {
	endpoint      string
	protocol      inferenceProtocol
	defaultStream bool
	billInput     bool
}

func classifyInference(method, path string) (inferenceSpec, bool) {
	if method != http.MethodPost {
		return inferenceSpec{}, false
	}
	switch path {
	case "/api/generate", "/v1/generate":
		return inferenceSpec{endpoint: "/api/generate", protocol: nativeNDJSON, defaultStream: true}, true
	case "/api/chat":
		return inferenceSpec{endpoint: "/api/chat", protocol: nativeNDJSON, defaultStream: true}, true
	case "/api/embed":
		return inferenceSpec{endpoint: path, protocol: jsonResponse, billInput: true}, true
	case "/v1/chat/completions":
		return inferenceSpec{endpoint: path, protocol: compatibleSSE}, true
	case "/v1/completions":
		return inferenceSpec{endpoint: path, protocol: compatibleSSE}, true
	case "/v1/responses":
		return inferenceSpec{endpoint: path, protocol: compatibleSSE}, true
	case "/v1/messages":
		return inferenceSpec{endpoint: path, protocol: compatibleSSE}, true
	case "/v1/embeddings":
		return inferenceSpec{endpoint: path, protocol: jsonResponse, billInput: true}, true
	default:
		return inferenceSpec{}, false
	}
}

func (s *Server) inference(w http.ResponseWriter, r *http.Request, spec inferenceSpec) {
	u, hash, ok := s.user(w, r)
	if !ok {
		return
	}
	err := s.Meter.PreflightUser(hash, u)
	if err != nil {
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			writeError(w, http.StatusPaymentRequired, "top up your balance before requesting inference")
			return
		}
		writeStoreError(w, err)
		return
	}
	release, acquired := s.Limits.Acquire(r.Context(), hash, guardrails.Limits{RatePerMinute: u.RateLimitPerMin, Concurrency: u.ConcurrencyLimit})
	if !acquired {
		writeError(w, http.StatusTooManyRequests, "request cancelled while waiting for a capacity slot")
		return
	}
	defer release()

	body, err := readBody(w, r, s.Config.MaxBodyBytes)
	if err != nil {
		s.noteFailure(r, "malformed_inference")
		return
	}
	stream := spec.defaultStream
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		s.noteFailure(r, "malformed_inference")
		writeError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	if raw, exists := fields["stream"]; exists {
		if err := json.Unmarshal(raw, &stream); err != nil {
			writeError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	var prepared []byte
	switch spec.protocol {
	case compatibleSSE:
		prepared, err = s.Ollama.PrepareCompatible(body, spec.endpoint, s.Config.SafetyMaxTokens)
	case jsonResponse:
		prepared, err = s.Ollama.PrepareModel(body)
	default:
		prepared, err = s.Ollama.PrepareNative(body, stream, s.Config.SafetyMaxTokens)
	}
	if err != nil {
		s.noteFailure(r, "malformed_inference")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Config.RequestTimeout)
	defer cancel()
	started := time.Now()
	reservedTokens := ollama.OutputLimit(prepared, s.Config.SafetyMaxTokens)
	if spec.billInput {
		// A tokenizer cannot produce more tokens than the bytes supplied to it.
		// JSON overhead makes this deliberately conservative for embeddings.
		reservedTokens = len(body)
	}
	tracker, err := s.Meter.Tracker(hash, u.BalanceMicroUSDC, started, reservedTokens)
	if err != nil {
		writeError(w, http.StatusPaymentRequired, "insufficient balance for requested inference limit")
		return
	}
	defer tracker.Cancel()
	upstreamRequest := r.WithContext(ctx)
	if spec.protocol == jsonResponse || !stream {
		s.jsonInference(w, upstreamRequest, spec, prepared, tracker, hash)
		return
	}
	s.streamInference(w, upstreamRequest, spec, prepared, tracker, hash)
}

// upstream sends the prepared request to Ollama. It returns nil after writing an
// error or redirect response; otherwise the caller must close resp.Body.
func (s *Server) upstream(w http.ResponseWriter, r *http.Request, spec inferenceSpec, body []byte) *http.Response {
	resp, err := s.Ollama.Do(r.Context(), r, spec.endpoint, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return nil
	}
	if resp.StatusCode >= 300 {
		forwardResponse(w, resp)
		_ = resp.Body.Close()
		return nil
	}
	return resp
}

func (s *Server) streamInference(w http.ResponseWriter, r *http.Request, spec inferenceSpec, body []byte, tracker *meter.Tracker, hash string) {
	resp := s.upstream(w, r, spec, body)
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header, false)
	flush, _ := w.(http.Flusher)
	wrote := false
	callback := func(raw []byte, event ollama.Event) error {
		n, err := w.Write(raw)
		wrote = wrote || n > 0
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
		tracker.Observe(event)
		if flush != nil {
			flush.Flush()
		}
		return nil
	}
	var usage ollama.Event
	var streamErr error
	if spec.protocol == nativeNDJSON {
		usage, streamErr = s.Ollama.StreamNDJSON(resp.Body, callback)
	} else {
		usage, streamErr = s.Ollama.StreamSSE(resp.Body, callback)
	}
	if streamErr != nil {
		s.Logger.Error("inference stream failed", "error", streamErr, "key", hash)
		if !wrote {
			writeError(w, http.StatusBadGateway, streamErr.Error())
			return
		}
	}
	if _, _, err := s.finishBilling(tracker, usage, streamErr != nil, spec.billInput); err != nil {
		s.Logger.Error("billing failed after stream", "error", err, "key", hash)
	}
}

func (s *Server) jsonInference(w http.ResponseWriter, r *http.Request, spec inferenceSpec, body []byte, tracker *meter.Tracker, hash string) {
	resp := s.upstream(w, r, spec, body)
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "unable to read Ollama response")
		return
	}
	event := ollama.EventFromJSON(data)
	tracker.Observe(event)
	if _, _, err := s.finishBilling(tracker, event, false, spec.billInput); err != nil {
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			writeError(w, http.StatusPaymentRequired, "insufficient balance for completed inference")
		} else {
			writeError(w, http.StatusInternalServerError, "unable to record usage")
		}
		return
	}
	copyResponseHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	if !isOllamaPath(r.URL.Path) {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}
	if spec, ok := classifyInference(r.Method, r.URL.Path); ok {
		s.inference(w, r, spec)
		return
	}
	u, hash, ok := s.user(w, r)
	if !ok {
		return
	}
	release, acquired := s.Limits.Acquire(r.Context(), hash, guardrails.Limits{RatePerMinute: u.RateLimitPerMin, Concurrency: u.ConcurrencyLimit})
	if !acquired {
		writeError(w, http.StatusTooManyRequests, "request cancelled while waiting for a capacity slot")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), s.Config.RequestTimeout)
	defer cancel()
	request := r.WithContext(ctx)
	s.Proxy.ServeHTTP(w, request)
}

func (s *Server) proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	s.Logger.Error("Ollama proxy failed", "error", err)
	writeError(w, http.StatusBadGateway, "Ollama proxy request failed")
}

func isOllamaPath(path string) bool {
	return path == "/" || path == "/api" || path == "/v1" || strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
}

func forwardResponse(w http.ResponseWriter, resp *http.Response) {
	copyResponseHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyResponseHeaders(dst, src http.Header, contentLength bool) {
	headers := src.Clone()
	ollama.RemoveHopByHop(headers)
	if !contentLength {
		headers.Del("Content-Length")
	}
	for key, values := range headers {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (s *Server) finishBilling(tracker *meter.Tracker, usage ollama.Event, partial, billInput bool) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return tracker.Finalize(ctx, usage, partial, billInput)
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (domain.User, string, bool) {
	key, err := auth.FromRequest(r)
	if err != nil {
		s.noteFailure(r, "authentication")
		writeError(w, http.StatusUnauthorized, "API key required")
		return domain.User{}, "", false
	}
	hash := auth.Hash(key)
	u, err := s.Store.GetUser(r.Context(), hash)
	if err != nil {
		s.noteFailure(r, "authentication")
		if errors.Is(err, ledger.ErrRevoked) || errors.Is(err, ledger.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid API key")
		} else {
			writeStoreError(w, err)
		}
		return domain.User{}, "", false
	}
	return u, hash, true
}

func (s *Server) noteFailure(r *http.Request, reason string) {
	identity := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		identity = host
	}
	if count := s.Failures.Record(identity); count >= 5 {
		s.Logger.Warn("repeated request failures", "reason", reason, "source", identity, "count", count)
	}
}

func requestLogging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &contextWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", wrapped.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type contextWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *contextWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *contextWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *contextWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *contextWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func decodeBody(w http.ResponseWriter, r *http.Request, target any, max int64) error {
	body, err := readBody(w, r, max)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be valid JSON")
		return err
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "request body is required")
		return nil, io.EOF
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unable to read request body")
		return nil, err
	}
	if int64(len(body)) > max {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, fmt.Errorf("body too large")
	}
	return body, nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, http.StatusPaymentRequired, "insufficient balance")
	case errors.Is(err, ledger.ErrConflict):
		writeError(w, http.StatusConflict, "top-up transaction is already associated with different payment data")
	default:
		writeError(w, http.StatusInternalServerError, "ledger operation failed")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
