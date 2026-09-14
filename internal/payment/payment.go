package payment

import (
	"encoding/json"
	"net/http"
	"strings"

	x402 "github.com/mark3labs/x402-go"
	"github.com/mark3labs/x402-go/encoding"
	x402http "github.com/mark3labs/x402-go/http"

	"github.com/farsoudi/payed-llm-inference/internal/config"
	"github.com/farsoudi/payed-llm-inference/internal/money"
)

type Service struct {
	Config      config.Config
	facilitator *x402http.FacilitatorClient
}

func New(cfg config.Config) *Service {
	return &Service{
		Config: cfg,
		facilitator: &x402http.FacilitatorClient{
			BaseURL:       cfg.FacilitatorURL,
			Client:        &http.Client{},
			Timeouts:      x402.DefaultTimeouts,
			Authorization: cfg.FacilitatorAuthorization,
		},
	}
}

type Settlement struct {
	Transaction string
	Payer       string
	Network     string
}

// Protect implements the x402 HTTP flow while leaving the database credit in
// the protected handler. Unlike the stock middleware, this avoids committing
// a successful response before the credit operation has run.
func (s *Service) Protect(amount int64, description string, next func(Settlement) http.Handler) http.Handler {
	chain := x402.BaseSepolia
	if s.Config.Network == "base" {
		chain = x402.BaseMainnet
	}
	requirement, err := x402.NewUSDCPaymentRequirement(x402.USDCRequirementConfig{
		Chain:             chain,
		Amount:            money.FormatUSDC(amount),
		RecipientAddress:  s.Config.PayTo,
		Description:       description,
		MaxTimeoutSeconds: 300,
	})
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "invalid payment configuration")
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		facilitator := s.facilitator
		if facilitator == nil {
			facilitator = New(s.Config).facilitator
		}
		requirement.Resource = resourceURL(r)
		if r.Header.Get("X-PAYMENT") == "" {
			sendPaymentRequired(w, []x402.PaymentRequirement{requirement})
			return
		}
		payload, err := encoding.DecodePayment(r.Header.Get("X-PAYMENT"))
		if err != nil || payload.X402Version != 1 {
			writeError(w, http.StatusBadRequest, "invalid x402 payment header")
			return
		}
		matching, err := x402.FindMatchingRequirement(payload, []x402.PaymentRequirement{requirement})
		if err != nil {
			sendPaymentRequired(w, []x402.PaymentRequirement{requirement})
			return
		}
		verified, err := facilitator.Verify(r.Context(), payload, *matching)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "payment verification unavailable")
			return
		}
		if verified == nil || !verified.IsValid {
			sendPaymentRequired(w, []x402.PaymentRequirement{requirement})
			return
		}
		settled, err := facilitator.Settle(r.Context(), payload, *matching)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "payment settlement unavailable")
			return
		}
		if settled == nil {
			writeError(w, http.StatusServiceUnavailable, "payment settlement returned no result")
			return
		}
		payer := settled.Payer
		if payer == "" && verified != nil {
			payer = verified.Payer
		}
		network := settled.Network
		if network == "" {
			network = matching.Network
		}
		if !settled.Success || strings.TrimSpace(settled.Transaction) == "" || strings.TrimSpace(payer) == "" || network != matching.Network {
			sendPaymentRequired(w, []x402.PaymentRequirement{requirement})
			return
		}
		encoded, err := encoding.EncodeSettlement(*settled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "unable to encode settlement")
			return
		}
		w.Header().Set("X-PAYMENT-RESPONSE", encoded)
		next(Settlement{Transaction: settled.Transaction, Payer: payer, Network: network}).ServeHTTP(w, r)
	})
}

func resourceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.RequestURI
}

func sendPaymentRequired(w http.ResponseWriter, requirements []x402.PaymentRequirement) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(x402.PaymentRequirementsResponse{X402Version: 1, Error: "Payment required for this resource", Accepts: requirements})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
