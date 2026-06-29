// codex-tls-proxy: TLS 指纹代理服务
//
// 复刻 Codex CLI（reqwest + native-tls）默认路径的 TLS 栈，为 sub2api 的 OpenAI
// OAuth 账号上游请求提供 TLS 指纹模拟。sub2api 把完整请求（含 headers/body/proxy）
// 转发给本代理，本代理用 reqwest + native-tls 栈通过指定 proxy 发出请求，并把上游
// 响应流式透传回 sub2api。
//
// codex CLI 默认 OpenAI 请求走 native-tls（macOS=Secure Transport, Linux=OpenSSL,
// Windows=SChannel），只有配置 CODEX_CA_CERTIFICATE/SSL_CERT_FILE 时才切到 rustls。
// 本代理对齐默认路径。
//
// 本代理不处理任何业务 header 逻辑（originator/User-Agent 等由 sub2api 设置），
// 只负责用正确的 TLS 栈发出请求。

mod handler;
mod proxy_client;

pub use handler::app;
pub use handler::ForwardRequest;
