use anyhow::{Context, Result, ensure};
use clap::Parser;
use fs2::FileExt;
use octomus_agent::{api, engine::App, store::Store};
use std::{net::SocketAddr, path::PathBuf, time::Duration};
#[derive(Parser)]
#[command(
    version,
    about = "Continuous repository improvement through reviewed pull requests"
)]
struct Args {
    #[arg(long, env = "OCTOMUS_DATA_DIR", default_value = ".octomus")]
    data_dir: PathBuf,
    #[arg(long, env = "OCTOMUS_LISTEN", default_value = "127.0.0.1:4200")]
    listen: SocketAddr,
    #[arg(long, env = "OCTOMUS_ASSETS", default_value = "web/build")]
    assets: PathBuf,
    #[arg(long, help = "Print configuration defaults and exit")]
    print_config: bool,
    #[arg(
        long,
        help = "Validate saved repository, authentication and model routes, then exit"
    )]
    doctor: bool,
    #[arg(long, conflicts_with_all = ["doctor", "print_config"], help = "Export a read-only JSON usage report from saved state and exit")]
    usage_report: bool,
}
#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "octomus_agent=info,tower_http=info".into()),
        )
        .init();
    let args = Args::parse();
    if args.print_config {
        println!(
            "{}",
            serde_json::to_string_pretty(&octomus_agent::config::Config::default())?
        );
        return Ok(());
    }
    if args.usage_report {
        println!(
            "{}",
            serde_json::to_string_pretty(&octomus_agent::report::usage_report(
                &args.data_dir.join("state.db")
            )?)?
        );
        return Ok(());
    }
    std::fs::create_dir_all(&args.data_dir)?;
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(&args.data_dir, std::fs::Permissions::from_mode(0o700))?;
    let data = std::fs::canonicalize(&args.data_dir)?;
    let lock = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(data.join("service.lock"))?;
    lock.try_lock_exclusive()
        .context("Another Octomus service is using this data directory")?;
    let app = App::new(Store::open(&data.join("state.db"))?, data);
    if args.doctor {
        println!(
            "{}",
            serde_json::to_string_pretty(&app.doctor(&app.config()?).await?)?
        );
        return Ok(());
    }
    let token=std::env::var("OCTOMUS_TOKEN").context("Set OCTOMUS_TOKEN to a random operator token of at least 32 characters (openssl rand -hex 32)")?;
    ensure!(
        token.len() >= 32,
        "OCTOMUS_TOKEN must contain at least 32 characters"
    );
    ensure!(
        args.assets.join("200.html").exists(),
        "Dashboard assets missing. Run npm ci and npm run build in web/, or set --assets"
    );
    app.recover()?;
    let worker = tokio::spawn(app.clone().run());
    let listener = tokio::net::TcpListener::bind(args.listen).await?;
    tracing::info!("Octomus listening on http://{}", args.listen);
    let stop = app.shutdown.clone();
    axum::serve(listener, api::router(app.clone(), &token, args.assets))
        .with_graceful_shutdown(async move {
            let mut terminate =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                    .expect("SIGTERM handler");
            tokio::select! {_=tokio::signal::ctrl_c()=>{},_=terminate.recv()=>{}}
            stop.cancel();
        })
        .await?;
    let _ = worker.await;
    for _ in 0..100 {
        if app.runtime.lock().unwrap().tasks.is_empty()
            && app.runtime.lock().unwrap().cycle.is_none()
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    Ok(())
}
