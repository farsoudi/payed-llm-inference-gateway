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
	Payments *payment.Service
	Logger   *slog.Logger
}

func New(cfg config.Config, store ledger.Store) *Server {
	cache := meter.NewBalanceCache()
	return &Server{
		Config:   cfg,
		Store:    store,
		Meter:    &meter.Meter{Store: store, Cache: cache, Config: cfg},
		Limits:   guardrails.New(),
		Failures: guardrails.NewFailureMonitor(),
		Ollama:   &ollama.Client{BaseURL: cfg.OllamaURL, Model: cfg.OllamaModel, HTTPClient: &http.Client{Timeout: cfg.RequestTimeout}},
		Payments: payment.New(cfg),
		Logger:   slog.Default(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/users/me", s.me)
	mux.HandleFunc("POST /v1/topups", s.topup)
	mux.HandleFunc("POST /v1/generate", s.generate)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return requestLogging(mux, s.Logger)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	_, hash, ok := s.user(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUser(r.Context(), hash)
	if err != nil {
		writeStoreError(w, err)
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
			balance, inserted, err := s.Store.CreditTopUp(r.Context(), domain.TopUp{KeyHash: hash, Amount: amount, Transaction: settlement.Transaction, Payer: settlement.Payer, Network: settlement.Network})
			if err != nil {
				writeStoreError(w, err)
				return
			}
			s.Meter.Credit(hash, balance)
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

func (s *Server) generate(w http.ResponseWriter, r *http.Request) { s.inference(w, r, "/api/generate") }
func (s *Server) chat(w http.ResponseWriter, r *http.Request)     { s.inference(w, r, "/api/chat") }

func (s *Server) inference(w http.ResponseWriter, r *http.Request, endpoint string) {
	_, hash, ok := s.user(w, r)
	if !ok {
		return
	}
	u, _, err := s.Meter.Preflight(r.Context(), hash)
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
	stream := true
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
	prepared, err := s.Ollama.Prepare(body, stream, s.Config.SafetyMaxTokens)
	if err != nil {
		s.noteFailure(r, "malformed_inference")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Config.RequestTimeout)
	defer cancel()
	started := time.Now()
	tracker := s.Meter.Tracker(hash, u.BalanceMicroUSDC, started)
	var lastRaw []byte
	partial := false
	if stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flush, _ := w.(http.Flusher)
		wrote := false
		result, streamErr := s.Ollama.Stream(ctx, endpoint, prepared, func(raw []byte, event ollama.Event) error {
			if err := tracker.Observe(event); err != nil {
				if errors.Is(err, ollama.ErrStopped) {
					partial = true
					_, _ = w.Write(append(raw, '\n'))
					wrote = true
					if flush != nil {
						flush.Flush()
					}
				}
				return err
			}
			_, err := w.Write(append(raw, '\n'))
			wrote = true
			if flush != nil {
				flush.Flush()
			}
			return err
		})
		if streamErr != nil {
			s.Logger.Error("inference stream failed", "error", streamErr, "key", hash)
			if !wrote {
				writeError(w, http.StatusBadGateway, streamErr.Error())
				return
			}
			partial = true
			metadata, _ := json.Marshal(map[string]any{"done": true, "done_reason": "upstream_error", "partial": true})
			_, _ = w.Write(append(metadata, '\n'))
			if flush != nil {
				flush.Flush()
			}
			if _, _, billingErr := s.finishBilling(hash, tracker, result.Usage, partial); billingErr != nil {
				s.Logger.Error("billing failed after failed stream", "error", billingErr, "key", hash)
			}
			return
		}
		if result.Stopped {
			partial = true
		}
		lastRaw = nil
		if partial {
			metadata := map[string]any{"done": true, "done_reason": "balance_depleted", "partial": true, "top_up_required": true}
			data, _ := json.Marshal(metadata)
			_, _ = w.Write(append(data, '\n'))
			if flush != nil {
				flush.Flush()
			}
		}
		if _, _, err := s.finishBilling(hash, tracker, result.Usage, partial); err != nil {
			s.Logger.Error("billing failed after stream", "error", err, "key", hash)
		}
		return
	}

	result, streamErr := s.Ollama.Stream(ctx, endpoint, prepared, func(raw []byte, event ollama.Event) error {
		lastRaw = append(lastRaw[:0], raw...)
		return tracker.Observe(event)
	})
	if streamErr != nil {
		_, _, _ = s.finishBilling(hash, tracker, result.Usage, true)
		writeError(w, http.StatusBadGateway, streamErr.Error())
		return
	}
	if result.Stopped {
		partial = true
	}
	if _, _, err := s.finishBilling(hash, tracker, result.Usage, partial); err != nil {
		writeError(w, http.StatusInternalServerError, "unable to record usage")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if len(lastRaw) == 0 {
		writeError(w, http.StatusBadGateway, "Ollama returned no response")
		return
	}
	_, _ = w.Write(lastRaw)
}

func (s *Server) finishBilling(key string, tracker *meter.Tracker, usage ollama.Event, partial bool) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return tracker.Finalize(ctx, usage, partial)
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	key, err := auth.FromRequest(r)
	if err != nil {
		s.noteFailure(r, "authentication")
		writeError(w, http.StatusUnauthorized, "API key required")
		return "", "", false
	}
	hash := auth.Hash(key)
	if _, err := s.Store.GetUser(r.Context(), hash); err != nil {
		s.noteFailure(r, "authentication")
		if errors.Is(err, ledger.ErrRevoked) || errors.Is(err, ledger.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid API key")
		} else {
			writeStoreError(w, err)
		}
		return "", "", false
	}
	return key, hash, true
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
	status int
}

func (w *contextWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *contextWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

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
