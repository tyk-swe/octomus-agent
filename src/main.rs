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
    #[arg(
        long,
        env = "OCTOMUS_ASSETS",
        help = "Override the embedded dashboard with a build directory"
    )]
    assets: Option<PathBuf>,
    #[arg(long, help = "Print configuration defaults and exit")]
    print_config: bool,
    #[arg(
        long,
        help = "Validate saved repository, authentication and model routes, then exit"
    )]
    doctor: bool,
    #[arg(
        long,
        requires = "doctor",
        help = "Check only audit prerequisites with --doctor"
    )]
    audit: bool,
    #[arg(long, conflicts_with_all = ["doctor", "print_config"], help = "Export a read-only JSON usage report from saved state and exit")]
    usage_report: bool,
    #[arg(long, value_name = "CYCLE_ID", conflicts_with_all = ["doctor", "print_config", "usage_report"], help = "Export read-only JSON run evidence for one saved cycle and exit")]
    export_run: Option<String>,
}
/// Prints one read-only export to stdout. Diagnostics stay on stderr.
fn print_json<T: serde::Serialize>(value: &T) -> Result<()> {
    println!("{}", serde_json::to_string_pretty(value)?);
    Ok(())
}
const STATE_DB: &str = "state.db";
#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "octomus_agent=info".into()),
        )
        .init();
    let args = Args::parse();
    if args.print_config {
        print_json(&octomus_agent::config::Config::default())?;
        return Ok(());
    }
    if args.usage_report {
        print_json(&octomus_agent::report::usage_report(
            &args.data_dir.join(STATE_DB),
        )?)?;
        return Ok(());
    }
    // Read-only export: no directory creation, permission change, service lock,
    // migration, App construction or worker start. Diagnostics stay on stderr.
    if let Some(cycle) = &args.export_run {
        print_json(&octomus_agent::evidence::export_run(
            &args.data_dir.join(STATE_DB),
            cycle,
        )?)?;
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
    let app = App::new(Store::open(&data.join(STATE_DB))?, data);
    if args.doctor {
        print_json(
            &app.doctor_for(
                &app.config()?,
                if args.audit {
                    octomus_agent::model::CycleMode::Audit
                } else {
                    octomus_agent::model::CycleMode::Execution
                },
            )
            .await?,
        )?;
        return Ok(());
    }
    let token = std::env::var(api::TOKEN_ENV).context(format!(
        "Set {} to a random operator token of at least 32 characters (openssl rand -hex 32)",
        api::TOKEN_ENV
    ))?;
    ensure!(
        token.len() >= 32,
        "{} must contain at least 32 characters",
        api::TOKEN_ENV
    );
    if let Some(assets) = &args.assets {
        ensure!(
            assets.join("200.html").is_file(),
            "Dashboard override missing 200.html; build the dashboard or correct --assets"
        );
    }
    if !args.listen.ip().is_loopback() {
        tracing::warn!(
            "Non-loopback listener {} exposes operator access. Use a loopback address and an SSH tunnel; the token grants full operator control.",
            args.listen
        );
    }
    let notifications = octomus_agent::notifications::start(
        &app,
        std::env::var(octomus_agent::notifications::WEBHOOK_ENV).ok(),
    )?;
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
    let _ = notifications.await;
    for _ in 0..100 {
        if app.runtime().drained() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    Ok(())
}
