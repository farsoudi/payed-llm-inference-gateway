package ollama

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

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
	RemoveHopByHop(upstream.Header)
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

// RemoveHopByHop drops connection-specific headers, including any named by the
// Connection header. Shared by the request and response forwarding paths.
func RemoveHopByHop(header http.Header) {
	for _, name := range strings.Split(header.Get("Connection"), ",") {
		header.Del(strings.TrimSpace(name))
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}
