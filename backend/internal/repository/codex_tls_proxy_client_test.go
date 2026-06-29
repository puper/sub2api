package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// codex_tls_proxy_client_test.go
//
// 测试 CodexTLSProxyClient.Forward 的请求构造、响应透传、body streaming、错误处理。

// newTestCodexTLSProxyClient 创建测试用客户端
func newTestCodexTLSProxyClient(t *testing.T, proxyURL string) *CodexTLSProxyClient {
	t.Helper()
	return &CodexTLSProxyClient{
		url:    proxyURL,
		client: &http.Client{Timeout: 0},
	}
}

func TestCodexTLSProxyClient_ForwardBasic(t *testing.T) {
	// 模拟 Rust 代理：接收 /forward 请求，解析后返回 200 + 透传 body
	var receivedForwardReq codexTLSProxyForwardRequest
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forward" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// 解析 /forward 请求体
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &receivedForwardReq); err != nil {
			t.Errorf("failed to unmarshal forward request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// 返回上游响应
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Custom", "custom-value")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: hello\n\n"))
	}))
	defer proxyServer.Close()

	client := newTestCodexTLSProxyClient(t, proxyServer.URL)

	// 构造上游请求
	upstreamReq, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	upstreamReq.Header.Set("Authorization", "Bearer token123")
	upstreamReq.Header.Set("Originator", "codex_cli_rs")
	upstreamReq.Host = "chatgpt.com"

	resp, err := client.Forward(context.Background(), upstreamReq, "http://proxy:8080")
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 验证响应
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom") != "custom-value" {
		t.Errorf("expected X-Custom header, got %q", resp.Header.Get("X-Custom"))
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", resp.Header.Get("Content-Type"))
	}

	// 验证 body
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "data: hello\n\n" {
		t.Errorf("unexpected body: %q", string(body))
	}

	// 验证转发请求的字段
	if receivedForwardReq.Method != "POST" {
		t.Errorf("expected method POST, got %s", receivedForwardReq.Method)
	}
	if receivedForwardReq.URL != "https://chatgpt.com/backend-api/codex/responses" {
		t.Errorf("unexpected URL: %s", receivedForwardReq.URL)
	}
	if receivedForwardReq.ProxyURL != "http://proxy:8080" {
		t.Errorf("unexpected proxy_url: %s", receivedForwardReq.ProxyURL)
	}
	if receivedForwardReq.Headers["Authorization"][0] != "Bearer token123" {
		t.Errorf("unexpected Authorization header: %v", receivedForwardReq.Headers["Authorization"])
	}
	if receivedForwardReq.Headers["Originator"][0] != "codex_cli_rs" {
		t.Errorf("unexpected Originator header: %v", receivedForwardReq.Headers["Originator"])
	}
}

func TestCodexTLSProxyClient_ForwardWithBody(t *testing.T) {
	var receivedForwardReq codexTLSProxyForwardRequest
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedForwardReq)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	}))
	defer proxyServer.Close()

	client := newTestCodexTLSProxyClient(t, proxyServer.URL)

	upstreamReq, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-4","stream":true}`))

	resp, err := client.Forward(context.Background(), upstreamReq, "")
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}

	// 验证 body 被 base64 编码
	decoded, err := base64.StdEncoding.DecodeString(receivedForwardReq.Body)
	if err != nil {
		t.Fatalf("failed to decode base64 body: %v", err)
	}
	if string(decoded) != `{"model":"gpt-4","stream":true}` {
		t.Errorf("unexpected decoded body: %q", string(decoded))
	}
}

func TestCodexTLSProxyClient_ForwardError502(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream connection refused"}`))
	}))
	defer proxyServer.Close()

	client := newTestCodexTLSProxyClient(t, proxyServer.URL)

	upstreamReq, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)

	_, err := client.Forward(context.Background(), upstreamReq, "")
	if err == nil {
		t.Fatal("expected error for 502 response, got nil")
	}
}

func TestCodexTLSProxyClient_HealthCheck(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer proxyServer.Close()

	client := newTestCodexTLSProxyClient(t, proxyServer.URL)
	if err := client.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

func TestCodexTLSProxyClient_HealthCheckFailure(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer proxyServer.Close()

	client := newTestCodexTLSProxyClient(t, proxyServer.URL)
	if err := client.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error for 500 health check, got nil")
	}
}
