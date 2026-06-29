// codex-tls-proxy: TLS 指纹代理服务
//
// 复刻 Codex CLI（reqwest + rustls）的 TLS 栈，为 sub2api 的 OpenAI OAuth 账号
// 上游请求提供 TLS 指纹模拟。sub2api 把完整请求（含 headers/body/proxy）转发
// 给本代理，本代理用 reqwest + rustls 栈通过指定 proxy 发出请求，并把上游响应
// 流式透传回 sub2api。
//
// 本代理不处理任何业务 header 逻辑（originator/User-Agent 等由 sub2api 设置），
// 只负责用正确的 TLS 栈发出请求。

mod handler;
mod proxy_client;

pub use handler::app;
pub use handler::ForwardRequest;
