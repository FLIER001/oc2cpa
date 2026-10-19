package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Aligned with App.useragent: opencode/<channel>/<version>/<client>.
const upstreamUserAgent = "opencode/latest/2.0.29/cli"

// The free tier does not require a real token.
const upstreamAuthHeader = "Bearer public"

var (
	httpClientsMu   sync.Mutex
	sharedClient    *http.Client
	sharedClientKey string
	proxyClients    = map[string]*http.Client{}
)

// sharedHTTPClient lazily builds (and rebuilds on config change) the upstream
// HTTP client. The connect timeout only bounds dial + response headers; once
// the upstream starts streaming, reads are bounded by the response-layer
// watchdogs, never cut short mid-generation.
func sharedHTTPClient() *http.Client {
	settingsSnapshot := readSettings()
	key := fmt.Sprintf("t=%d|p=%s", settingsSnapshot.ConnectTimeoutMS, readHostProxy())
	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()
	if sharedClient != nil && sharedClientKey == key {
		return sharedClient
	}
	sharedClient = buildClient(readHostProxy(), settingsSnapshot.ConnectTimeoutMS)
	sharedClientKey = key
	return sharedClient
}

// clientForAuth returns (and caches) an HTTP client honoring the optional
// proxy_url stored in the auth file, so multiple auth files can use different
// exit IPs the way the original deployment ran one oc2cap instance per IP.
// An auth without its own proxy falls back to the host's global proxy-url.
// socks5/socks5h proxies are ignored by the stdlib transport and pass through;
// unsupported schemes are logged and skipped.
func clientForAuth(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		proxyURL = readHostProxy()
	}
	settingsSnapshot := readSettings()
	key := fmt.Sprintf("p=%s|t=%d", proxyURL, settingsSnapshot.ConnectTimeoutMS)
	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()
	if client, ok := proxyClients[key]; ok {
		return client
	}
	client := buildClient(proxyURL, settingsSnapshot.ConnectTimeoutMS)
	proxyClients[key] = client
	return client
}

func buildClient(proxyURL string, connectTimeoutMS int) *http.Client {
	timeout := time.Duration(connectTimeoutMS) * time.Millisecond
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.TLSHandshakeTimeout = timeout
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		return dialer.DialContext(ctx, network, addr)
	}
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil && isHTTPishProxy(parsed.Scheme) {
			transport.Proxy = http.ProxyURL(parsed)
		} else {
			fsDebugLog("ignoring unsupported proxy scheme for " + proxyURL)
		}
	}
	return &http.Client{Transport: transport}
}

func isHTTPishProxy(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// upstreamHeaders mirrors zen.fetchUpstream: the upstream merges sessions on
// x-opencode-session and validates the OpenCode client identity headers.
func upstreamHeaders(sessionID string) map[string]string {
	return map[string]string{
		"Accept":                "text/event-stream",
		"Content-Type":          "application/json",
		"Authorization":         upstreamAuthHeader,
		"User-Agent":            upstreamUserAgent,
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    sessionID,
		"x-opencode-session-id": sessionID,
		"x-session-affinity":    sessionID,
		"X-Session-Id":          sessionID,
	}
}

func chatURL() string {
	return readSettings().UpstreamBaseURL + "/chat/completions"
}

// newUpstreamChatRequest builds the streaming chat request.
func newUpstreamChatRequest(ctx context.Context, built *upstreamRequest) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(), bytes.NewReader(built.Body))
	if err != nil {
		return nil, err
	}
	for key, value := range upstreamHeaders(built.SessionID) {
		request.Header.Set(key, value)
	}
	return request, nil
}

// newUpstreamModelsRequest builds the model discovery request.
func newUpstreamModelsRequest(modelsURL string) (*http.Request, error) {
	request, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", upstreamAuthHeader)
	request.Header.Set("User-Agent", upstreamUserAgent)
	return request, nil
}

func fsDebugLog(message string) {
	// The host captures plugin stderr, so anything printed here surfaces in
	// host logs prefixed with the plugin name.
	fmt.Fprintln(os.Stderr, "[oc2cap] "+message)
}

// ioReadAll small helper for bounded model-list reads.
func readAllBounded(body io.Reader, maxBytes int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, maxBytes))
}
