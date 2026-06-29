use std::net::SocketAddr;

use codex_tls_proxy::app;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
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
