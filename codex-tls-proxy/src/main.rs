use std::net::SocketAddr;

use codex_tls_proxy::app;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() {
    // reqwest 默认走 native-tls（macOS=Secure Transport, Linux=OpenSSL, Windows=SChannel），
    // 对齐 codex CLI 默认 OpenAI 请求路径。codex CLI 只有在配置了
    // CODEX_CA_CERTIFICATE/SSL_CERT_FILE 时才切到 rustls，本代理对齐默认路径。
    // native-tls 不需要手动安装 crypto provider。

    // 默认日志级别 info：未设置 RUST_LOG 时也能看到启动日志和 /forward 请求日志；
    // 设置了 RUST_LOG 则以其为准。
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")))
        .init();

    // 监听地址，默认 127.0.0.1:18900
    let addr: SocketAddr = std::env::var("CODEX_TLS_PROXY_ADDR")
        .unwrap_or_else(|_| "127.0.0.1:18900".to_string())
        .parse()
        .expect("invalid CODEX_TLS_PROXY_ADDR");

    let listener = tokio::net::TcpListener::bind(addr)
        .await
        .unwrap_or_else(|e| panic!("failed to bind {addr}: {e}"));

    tracing::info!("codex-tls-proxy listening on {addr}");

    axum::serve(listener, app())
        .with_graceful_shutdown(shutdown_signal())
        .await
        .unwrap_or_else(|e| panic!("server error: {e}"));
}

async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("failed to install signal handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }

    tracing::info!("shutdown signal received");
}
