package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// codex_tls_proxy_upstream_test.go
//
// 测试 codexTLSProxyUpstream 装饰器：
// - 未启用时行为与原始 HTTPUpstream 一致
// - 启用时 chatgpt.com / auth.openai.com 请求走 Rust 代理（Do 和 DoWithTLS 都拦截）
// - 非 OpenAI 域名请求走原始 HTTPUpstream
// - DoWithTLS 对 OpenAI 域名也走 Rust 代理（不经过 utls profile）

// stubHTTPUpstream 测试用 HTTPUpstream 桩
type stubHTTPUpstream struct {
	doCalled        bool
	doWithTLSCalled bool
	lastReq         *http.Request
	lastProxyURL    string
}

func (s *stubHTTPUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	s.doCalled = true
	s.lastReq = req
	s.lastProxyURL = proxyURL
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("stub-do-response")),
		Header:     make(http.Header),
	}, nil
}

func (s *stubHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	s.doWithTLSCalled = true
	s.lastReq = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("stub-dotls-response")),
		Header:     make(http.Header),
	}, nil
}

func TestCodexTLSProxyUpstream_Disabled(t *testing.T) {
	// 确保 CODEX_TLS_PROXY_ENABLED 未设置
	os.Unsetenv("CODEX_TLS_PROXY_ENABLED")
	// 重置 once 以重新读取环境变量
	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	cfg := &config.Config{}
	upstream := NewCodexTLSProxyHTTPUpstream(cfg)

	// 未启用时应返回原始 HTTPUpstream（非装饰器类型）
	if _, ok := upstream.(*codexTLSProxyUpstream); ok {
		t.Fatal("expected original HTTPUpstream when disabled, got codexTLSProxyUpstream")
	}
}

func TestCodexTLSProxyUpstream_EnabledChatGPTRequest(t *testing.T) {
	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")

	// 模拟 Rust 代理
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forward" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("data: from-rust-proxy\n\n"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer proxyServer.Close()

	os.Setenv("CODEX_TLS_PROXY_URL", proxyServer.URL)
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")

	// 重置 once 以重新读取环境变量
	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	cfg := &config.Config{}
	upstream := NewCodexTLSProxyHTTPUpstream(cfg)

	// 构造 chatgpt.com 请求（OpenAI OAuth）
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Host = "chatgpt.com"

	resp, err := upstream.Do(req, "http://proxy:8080", 1, 10)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "data: from-rust-proxy\n\n" {
		t.Errorf("expected response from rust proxy, got %q", string(body))
	}
}

func TestCodexTLSProxyUpstream_EnabledNonChatGPTRequest(t *testing.T) {
	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")

	// 重置 once
	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	// 创建装饰器，包装 stub
	stub := &stubHTTPUpstream{}
	upstream := &codexTLSProxyUpstream{
		inner:       stub,
		proxyClient: &CodexTLSProxyClient{url: "http://127.0.0.1:18900", client: &http.Client{}},
		enabled:     true,
	}

	// 非 OpenAI 域名请求应走原始 HTTPUpstream
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)

	resp, err := upstream.Do(req, "", 1, 10)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !stub.doCalled {
		t.Error("expected stub.Do to be called for non-OpenAI request")
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "stub-do-response" {
		t.Errorf("expected stub response, got %q", string(body))
	}
}

func TestCodexTLSProxyUpstream_DoWithTLS_OpenAIDomain(t *testing.T) {
	os.Setenv("CODEX_TLS_PROXY_ENABLED", "true")
	defer os.Unsetenv("CODEX_TLS_PROXY_ENABLED")

	// 模拟 Rust 代理
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forward" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("from-rust-proxy-dotls"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer proxyServer.Close()

	os.Setenv("CODEX_TLS_PROXY_URL", proxyServer.URL)
	defer os.Unsetenv("CODEX_TLS_PROXY_URL")

	codexTLSProxyEnvOnce = sync.Once{}
	loadCodexTLSProxyEnv()

	stub := &stubHTTPUpstream{}
	upstream := &codexTLSProxyUpstream{
		inner:       stub,
		proxyClient: &CodexTLSProxyClient{url: strings.TrimRight(codexTLSProxyURL, "/"), client: &http.Client{}},
		enabled:     true,
	}

	// DoWithTLS 对 chatgpt.com 请求应走 Rust 代理，不调用 stub.DoWithTLS
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Host = "chatgpt.com"

	resp, err := upstream.DoWithTLS(req, "", 1, 10, &tlsfingerprint.Profile{Name: "test"})
	if err != nil {
		t.Fatalf("DoWithTLS failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if stub.doWithTLSCalled {
		t.Error("expected stub.DoWithTLS NOT to be called for chatgpt.com request")
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "from-rust-proxy-dotls" {
		t.Errorf("expected response from rust proxy, got %q", string(body))
	}
}

func TestCodexTLSProxyUpstream_DoWithTLS_NonOpenAIDomain(t *testing.T) {
	stub := &stubHTTPUpstream{}
	upstream := &codexTLSProxyUpstream{
		inner:       stub,
		proxyClient: &CodexTLSProxyClient{url: "http://127.0.0.1:18900", client: &http.Client{}},
		enabled:     true,
	}

	// DoWithTLS 对非 OpenAI 域名应委托给原始 HTTPUpstream（Anthropic utls 路径）
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)

	resp, err := upstream.DoWithTLS(req, "", 1, 10, &tlsfingerprint.Profile{Name: "claude_cli_v2"})
	if err != nil {
		t.Fatalf("DoWithTLS failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !stub.doWithTLSCalled {
		t.Error("expected stub.DoWithTLS to be called for non-OpenAI request")
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "stub-dotls-response" {
		t.Errorf("expected stub DoWithTLS response, got %q", string(body))
	}
}

func TestCodexTLSProxyUpstream_ShouldForwardToProxy(t *testing.T) {
	u := &codexTLSProxyUpstream{enabled: true}

	tests := []struct {
		name string
		host string
		url  string
		want bool
	}{
		{"chatgpt.com via Host", "chatgpt.com", "https://chatgpt.com/backend-api/codex/responses", true},
		{"chatgpt.com via URL", "", "https://chatgpt.com/backend-api/codex/responses", true},
		{"auth.openai.com via Host", "auth.openai.com", "https://auth.openai.com/api/accounts/v1/user-auth-credential/whoami", true},
		{"auth.openai.com via URL", "", "https://auth.openai.com/api/accounts/v1/user-auth-credential/whoami", true},
		{"api.openai.com", "", "https://api.openai.com/v1/responses", false},
		{"api.anthropic.com", "", "https://api.anthropic.com/v1/messages", false},
		{"chatgpt.com with port", "chatgpt.com:443", "https://chatgpt.com/backend-api/codex/responses", true},
		{"empty host", "", "", false},
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

			if got := u.shouldForwardToProxy(req); got != tt.want {
				t.Errorf("shouldForwardToProxy() = %v, want %v", got, tt.want)
			}
		})
	}
}
