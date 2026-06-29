package httpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// codex_tls_proxy_transport_test.go
//
// 测试 codexTLSProxyTransport：
// - isOpenAIDomain 域名判断
// - wrapClientWithCodexTLSProxy 包装行为
// - GetCodexTLSProxyClient 启用/未启用时的行为
// - RoundTrip 转发逻辑（OpenAI 域名走代理，非 OpenAI 域名走原始 transport）

func resetCodexTLSProxyConfig() {
	codexTLSProxyTransportOnce = sync.Once{}
}

func TestIsOpenAIDomain(t *testing.T) {
	tests := []struct {
		name string
		host string
		url  string
		want bool
	}{
		{"chatgpt.com via Host", "chatgpt.com", "https://chatgpt.com/backend-api/codex/responses", true},
		{"chatgpt.com via URL", "", "https://chatgpt.com/backend-api/wham/usage", true},
		{"auth.openai.com via Host", "auth.openai.com", "https://auth.openai.com/api/accounts", true},
		{"auth.openai.com via URL", "", "https://auth.openai.com/api/accounts", true},
		{"api.openai.com", "", "https://api.openai.com/v1/responses", false},
		{"api.anthropic.com", "", "https://api.anthropic.com/v1/messages", false},
		{"chatgpt.com with port", "chatgpt.com:443", "https://chatgpt.com:443/backend-api", true},
		{"CHATGPT.COM uppercase", "CHATGPT.COM", "https://chatgpt.com/backend-api", true},
		{"empty", "", "", false},
		{"nil req", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.url != "" {
				req, _ = http.NewRequest(http.MethodPost, tt.url, nil)
				if tt.host != "" {
					req.Host = tt.host
				}
			}
			if got := isOpenAIDomain(req); got != tt.want {
				t.Errorf("isOpenAIDomain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetCodexTLSProxyClient_Disabled(t *testing.T) {
	os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	resetCodexTLSProxyConfig()

	client, err := GetCodexTLSProxyClient(Options{Timeout: 0})
	if err != nil {
		t.Fatalf("GetCodexTLSProxyClient failed: %v", err)
	}

	// 未启用时应返回原始 client（transport 不是 codexTLSProxyTransport）
	if _, ok := client.Transport.(*codexTLSProxyTransport); ok {
		t.Error("expected original transport when disabled")
	}
}

func TestGetCodexTLSProxyClient_Enabled(t *testing.T) {
	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	os.Setenv("CODEX_TLS_PROXY_URL", "http://127.0.0.1:18900")
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")
	resetCodexTLSProxyConfig()

	client, err := GetCodexTLSProxyClient(Options{ProxyURL: "http://proxy:8080"})
	if err != nil {
		t.Fatalf("GetCodexTLSProxyClient failed: %v", err)
	}

	// 启用时应返回包装后的 transport
	transport, ok := client.Transport.(*codexTLSProxyTransport)
	if !ok {
		t.Fatal("expected codexTLSProxyTransport when enabled")
	}
	if transport.upstreamProxy != "http://proxy:8080" {
		t.Errorf("expected upstream proxy http://proxy:8080, got %s", transport.upstreamProxy)
	}
}

func TestCodexTLSProxyTransport_RoundTrip_OpenAIDomain(t *testing.T) {
	// 模拟 Rust 代理
	var receivedForwardReq codexTLSProxyForwardRequest
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forward" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedForwardReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer proxyServer.Close()

	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	os.Setenv("CODEX_TLS_PROXY_URL", proxyServer.URL)
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")
	resetCodexTLSProxyConfig()

	// 原始 transport（用于非 OpenAI 请求的 fallback）
	innerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("inner-response"))
	}))
	defer innerServer.Close()

	transport := &codexTLSProxyTransport{
		inner:        http.DefaultTransport,
		upstreamProxy: "http://proxy:8080",
		rustProxyURL:  strings.TrimRight(proxyServer.URL, "/"),
		client:       &http.Client{Timeout: 0},
	}

	// chatgpt.com 请求应走 Rust 代理
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/wham/usage", strings.NewReader(`{"key":"value"}`))
	req.Host = "chatgpt.com"

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("expected response from rust proxy, got %q", string(body))
	}

	// 验证转发请求的字段
	if receivedForwardReq.Method != "POST" {
		t.Errorf("expected method POST, got %s", receivedForwardReq.Method)
	}
	if receivedForwardReq.ProxyURL != "http://proxy:8080" {
		t.Errorf("expected proxy_url http://proxy:8080, got %s", receivedForwardReq.ProxyURL)
	}
	// 验证 body 被 base64 编码
	decoded, _ := base64.StdEncoding.DecodeString(receivedForwardReq.Body)
	if string(decoded) != `{"key":"value"}` {
		t.Errorf("unexpected decoded body: %q", string(decoded))
	}
}

func TestCodexTLSProxyTransport_RoundTrip_NonOpenAIDomain(t *testing.T) {
	// 非 OpenAI 域名应走原始 transport
	innerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("inner-response"))
	}))
	defer innerServer.Close()

	transport := &codexTLSProxyTransport{
		inner:        http.DefaultTransport,
		upstreamProxy: "",
		rustProxyURL:  "http://127.0.0.1:18900",
		client:       &http.Client{Timeout: 0},
	}

	// 非 OpenAI 域名请求应走原始 transport（innerServer）
	req, _ := http.NewRequest(http.MethodGet, innerServer.URL+"/test", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "inner-response" {
		t.Errorf("expected inner response, got %q", string(body))
	}
}

func TestCodexTLSProxyTransport_RoundTrip_502Error(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream failed"}`))
	}))
	defer proxyServer.Close()

	transport := &codexTLSProxyTransport{
		inner:        http.DefaultTransport,
		upstreamProxy: "",
		rustProxyURL:  strings.TrimRight(proxyServer.URL, "/"),
		client:       &http.Client{Timeout: 0},
	}

	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/settings", nil)
	req.Host = "chatgpt.com"

	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error for 502 response, got nil")
	}
}

func TestHealthCheckCodexTLSProxy_Disabled(t *testing.T) {
	os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	resetCodexTLSProxyConfig()

	// 未启用时应返回 nil（无需检查）
	if err := HealthCheckCodexTLSProxy(context.Background()); err != nil {
		t.Errorf("expected nil when disabled, got %v", err)
	}
}

func TestHealthCheckCodexTLSProxy_Enabled(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer proxyServer.Close()

	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	os.Setenv("CODEX_TLS_PROXY_URL", proxyServer.URL)
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")
	resetCodexTLSProxyConfig()

	if err := HealthCheckCodexTLSProxy(context.Background()); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}
