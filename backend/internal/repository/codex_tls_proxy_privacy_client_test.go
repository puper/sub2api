package repository

import (
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

// codex_tls_proxy_privacy_client_test.go
//
// 测试 CodexTLSProxyPrivacyClientFactory：
// - 未启用时返回原始 CreatePrivacyReqClient 行为
// - 启用时请求走 Rust 代理

func TestCodexTLSProxyPrivacyClientFactory_Disabled(t *testing.T) {
	os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	client, err := CodexTLSProxyPrivacyClientFactory("")
	if err != nil {
		t.Fatalf("factory failed: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestCodexTLSProxyPrivacyClientFactory_Enabled(t *testing.T) {
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
		w.Write([]byte(`{"plan_type":"plus"}`))
	}))
	defer proxyServer.Close()

	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	os.Setenv("CODEX_TLS_PROXY_URL", proxyServer.URL)
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")
	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	client, err := CodexTLSProxyPrivacyClientFactory("http://proxy:8080")
	if err != nil {
		t.Fatalf("factory failed: %v", err)
	}

	// 用 req.Client 发请求到 chatgpt.com
	resp, err := client.R().
		SetHeader("Authorization", "Bearer test-token").
		SetHeader("Accept", "application/json").
		Get("https://chatgpt.com/backend-api/subscriptions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		t.Errorf("expected success status, got %d", resp.StatusCode)
	}

	// 验证转发请求的字段
	if receivedForwardReq.Method != "GET" {
		t.Errorf("expected method GET, got %s", receivedForwardReq.Method)
	}
	if receivedForwardReq.URL != "https://chatgpt.com/backend-api/subscriptions" {
		t.Errorf("unexpected URL: %s", receivedForwardReq.URL)
	}
	if receivedForwardReq.ProxyURL != "http://proxy:8080" {
		t.Errorf("expected proxy_url http://proxy:8080, got %s", receivedForwardReq.ProxyURL)
	}
	// 验证 Authorization header 被透传
	if auths, ok := receivedForwardReq.Headers["Authorization"]; !ok || len(auths) == 0 || auths[0] != "Bearer test-token" {
		t.Errorf("expected Authorization header to be forwarded, got %v", receivedForwardReq.Headers["Authorization"])
	}
}

func TestForwardViaRustProxy_BodyEncoding(t *testing.T) {
	// 模拟 Rust 代理
	var receivedBody string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fwdReq struct {
			Method   string              `json:"method"`
			URL      string              `json:"url"`
			ProxyURL string              `json:"proxy_url"`
			Headers  map[string][]string `json:"headers"`
			Body     string              `json:"body"`
		}
		_ = json.Unmarshal(body, &fwdReq)
		receivedBody = fwdReq.Body
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer proxyServer.Close()

	// 构造请求
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume",
		strings.NewReader(`{"redeem_request_id":"test-123"}`))
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")

	rustClient := &http.Client{Timeout: 0}
	resp, err := forwardViaRustProxy(req, rustClient, strings.TrimRight(proxyServer.URL, "/"), "")
	if err != nil {
		t.Fatalf("forwardViaRustProxy failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 验证 body 被 base64 编码
	decoded, err := base64.StdEncoding.DecodeString(receivedBody)
	if err != nil {
		t.Fatalf("failed to decode base64 body: %v", err)
	}
	if string(decoded) != `{"redeem_request_id":"test-123"}` {
		t.Errorf("unexpected decoded body: %q", string(decoded))
	}
}
