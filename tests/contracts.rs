//! Actual pinned clients with a synthetic loopback provider; no account/model access.
use anyhow::{Context, Result, ensure};
use octomus_agent::{
    config::{Backend, Config, Route},
    process::{self, GroupChild},
    runner::Runner,
    schemas,
    store::Store,
};
use serde_json::{Value, json};
use std::{os::unix::fs::PermissionsExt, path::Path, time::Duration};
use tokio_util::sync::CancellationToken;

async fn contract(backend: Backend, binary: &str) -> Result<()> {
    let temp = tempfile::tempdir()?;
    let root = temp.path();
    let workspace = root.join("workspace");
    std::fs::create_dir(&workspace)?;
    let provider = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/provider.py");
    let _provider = GroupChild::new(
        process::command("python3", root)
            .arg(&provider)
            .arg(root)
            .spawn()?,
    );
    for _ in 0..100 {
        if root.join("provider-port").exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let port = std::fs::read_to_string(root.join("provider-port"))?;
    let wrapper = root.join("client");
    let provider_config = root.join("provider.json");
    std::fs::write(&provider_config,json!({"enabled_providers":["contract"],"provider":{"contract":{"npm":"@ai-sdk/openai-compatible","name":"Contract","options":{"baseURL":format!("http://127.0.0.1:{port}/v1"),"apiKey":"unused-contract-placeholder"},"models":{"contract-model":{"name":"Contract model","limit":{"context":8192,"output":1024},"tool_call":true,"variants":{"high":{"temperature":0.1}}}}}}}).to_string())?;
    let codex_home = root.join("codex-home");
    std::fs::create_dir(&codex_home)?;
    std::fs::write(
        codex_home.join("config.toml"),
        format!(
            "model_provider = \"contract\"\n[model_providers.contract]\nname = \"Contract\"\nbase_url = \"http://127.0.0.1:{port}/v1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\n"
        ),
    )?;
    // Isolate only the child environment. Never read or modify operator account state.
    std::fs::write(
        &wrapper,
        format!(
            "#!/usr/bin/env python3\nimport os,sys\nroot={}\nenv={{k:v for k,v in os.environ.items() if k in ['PATH','LANG','OPENCODE_SERVER_USERNAME','OPENCODE_SERVER_PASSWORD','OPENCODE_CONFIG_CONTENT','OPENCODE_DISABLE_PROJECT_CONFIG','OPENCODE_DISABLE_AUTOUPDATE','OPENCODE_DISABLE_AUTOCOMPACT','OPENCODE_DISABLE_TERMINAL_TITLE']}}\nenv['HOME']=root\nenv['CODEX_HOME']=root+'/codex-home'\nfor key in ['XDG_CONFIG_HOME','XDG_DATA_HOME','XDG_STATE_HOME','XDG_CACHE_HOME']:\n env[key]=root+'/'+key\nenv['OPENCODE_CONFIG']={}\nenv['OPENCODE_DISABLE_MODELS_FETCH']='true'\nbinary={}\nos.execve(binary,[binary]+sys.argv[1:],env)\n",
            json!(root),
            json!(provider_config),
            json!(binary)
        ),
    )?;
    std::fs::set_permissions(&wrapper, std::fs::Permissions::from_mode(0o755))?;
    let version = process::run_machine(
        wrapper.to_str().unwrap(),
        &["--version"],
        root,
        60,
        &CancellationToken::new(),
    )
    .await?;
    let expected = if backend == Backend::Codex {
        format!("codex-cli {}", octomus_agent::codex::TESTED_VERSION)
    } else {
        octomus_agent::opencode::PROTOCOL_VERSION.into()
    };
    ensure!(
        version.trim() == expected,
        "Pinned contract requires {expected}, got {version}"
    );
    if backend == Backend::Codex {
        let generated = root.join("schemas");
        process::run_machine(
            wrapper.to_str().unwrap(),
            &[
                "app-server",
                "generate-json-schema",
                "--out",
                generated.to_str().unwrap(),
            ],
            root,
            60,
            &CancellationToken::new(),
        )
        .await?;
        for name in [
            "ThreadStartParams",
            "ThreadResumeParams",
            "TurnStartParams",
            "TurnInterruptParams",
        ] {
            let file = generated.join("v2").join(format!("{name}.json"));
            let schema: Value = serde_json::from_slice(
                &std::fs::read(&file)
                    .with_context(|| format!("Missing generated contract {}", file.display()))?,
            )?;
            ensure!(
                schema["properties"].is_object(),
                "Generated request schema changed for {name}"
            );
        }
    }
    let config = Config {
        codex_binary: wrapper.to_string_lossy().into_owned(),
        opencode_binary: wrapper.to_string_lossy().into_owned(),
        session_timeout_seconds: 30,
        command_timeout_seconds: 60,
        ..Default::default()
    };
    let route = if backend == Backend::Codex {
        Route::new("gpt-6-astra", "low")
    } else {
        Route {
            backend,
            model: "contract-model".into(),
            effort: String::new(),
            provider: Some("contract".into()),
            variant: Some("high".into()),
        }
    };
    let store = Store::open(&root.join("state.db"))?;
    let cancel = CancellationToken::new();
    let mut client = Runner::connect(
        backend,
        &config,
        &workspace,
        store.clone(),
        "contract",
        cancel.clone(),
    )
    .await?;
    if let Runner::OpenCode(server) = &client {
        let spec = server.protocol_schema(&workspace).await?;
        ensure!(
            spec["openapi"]
                .as_str()
                .is_some_and(|s| s.starts_with("3.")),
            "Missing OpenAPI schema"
        );
        for path in [
            "/session",
            "/session/{sessionID}/message",
            "/session/{sessionID}/abort",
        ] {
            ensure!(
                spec["paths"][path]["post"].is_object(),
                "Pinned OpenCode request contract missing {path}"
            );
        }
    }
    let session = client.start(&route, &workspace, None).await?;
    if backend == Backend::Codex {
        let resumed = client.start(&route, &workspace, Some(&session)).await;
        ensure!(
            resumed
                .as_ref()
                .err()
                .is_some_and(|error| format!("{error:#}").contains("no rollout found")),
            "Pinned Codex unexpectedly resumed a thread before its first turn"
        );
    }
    let answer = client
        .turn(
            &session,
            &route,
            &workspace,
            "Return the controlled contract result.",
            None,
        )
        .await?;
    ensure!(
        answer.contains("Controlled contract result"),
        "Missing completed turn result"
    );
    let structured = client
        .turn(
            &session,
            &route,
            &workspace,
            "Return the controlled review result.",
            Some(schemas::review_schema()),
        )
        .await?;
    schemas::validate(
        &serde_json::from_str(&structured)?,
        &schemas::review_schema(),
    )?;
    drop(client);
    let mut client = Runner::connect(
        backend,
        &config,
        &workspace,
        store,
        "contract",
        cancel.clone(),
    )
    .await?;
    ensure!(
        client.start(&route, &workspace, Some(&session)).await? == session,
        "Resume changed session identity"
    );
    let turn = client.turn(&session, &route, &workspace, "CANCEL_CONTRACT_TURN", None);
    tokio::pin!(turn);
    let interrupt = async {
        for _ in 0..200 {
            if root.join("turn-entered").exists() {
                cancel.cancel();
                return Ok::<_, anyhow::Error>(());
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        anyhow::bail!("Controlled cancellation request never reached the provider")
    };
    let (result, interrupted) = tokio::time::timeout(Duration::from_secs(20), async {
        tokio::join!(turn, interrupt)
    })
    .await?;
    interrupted?;
    ensure!(
        result.is_err(),
        "Cancelled turn returned successful evidence"
    );
    Ok(())
}
#[tokio::test]
#[ignore = "requires the pinned real Codex binary; synthetic provider only"]
async fn pinned_codex_contract() -> Result<()> {
    contract(
        Backend::Codex,
        &std::env::var("OCTOMUS_CONTRACT_CODEX_BINARY")?,
    )
    .await
}
#[tokio::test]
#[ignore = "requires the pinned real OpenCode binary; synthetic provider only"]
async fn pinned_opencode_contract() -> Result<()> {
    contract(
        Backend::Opencode,
        &std::env::var("OCTOMUS_CONTRACT_OPENCODE_BINARY")?,
    )
    .await
}
