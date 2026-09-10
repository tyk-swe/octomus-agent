//! Real owned processes talking to deterministic HTTP/SSE peers, without model calls.
use octomus_agent::{
    config::{Backend, Config, Route},
    model::Session,
    opencode::OpenCode,
    runner::{Runners, validate_route},
    schemas,
    store::{Admission, Store},
};
use serde_json::{Value, json};
use std::{os::unix::fs::PermissionsExt, path::PathBuf, time::Duration};
use tokio_util::sync::CancellationToken;

fn route() -> Route {
    Route {
        backend: Backend::Opencode,
        model: "fixture-model".into(),
        effort: String::new(),
        provider: Some("fixture".into()),
        variant: Some("high".into()),
    }
}
struct Fixture {
    temp: tempfile::TempDir,
    workspace: PathBuf,
    config: Config,
    store: Store,
}
impl Fixture {
    fn new() -> Self {
        let temp = tempfile::tempdir().unwrap();
        let workspace = temp.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let wrapper = temp.path().join("opencode");
        let fixtures = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures");
        std::fs::write(&wrapper, format!("#!/usr/bin/env python3\nimport os, runpy, sys\nos.environ['OCTOMUS_FIXTURE'] = {}\nsys.path.insert(0, {})\nrunpy.run_path({}, run_name='__main__')\n",
            json!(temp.path()), json!(fixtures), json!(fixtures.join("opencode.py")))).unwrap();
        std::fs::set_permissions(&wrapper, std::fs::Permissions::from_mode(0o755)).unwrap();
        let config = Config {
            opencode_binary: wrapper.to_string_lossy().into_owned(),
            codex_binary: "/no-codex-installed".into(),
            session_timeout_seconds: 10,
            command_timeout_seconds: 2,
            ..Config::default()
        };
        let store = Store::open(&temp.path().join("state.db")).unwrap();
        Self {
            temp,
            workspace,
            config,
            store,
        }
    }
    fn mode(&self, mode: &str) {
        std::fs::write(self.temp.path().join("opencode-mode"), mode).unwrap();
    }
    async fn connect(&self, cancel: CancellationToken) -> anyhow::Result<OpenCode> {
        OpenCode::connect(
            &self.config,
            &self.workspace,
            self.store.clone(),
            "fixture",
            cancel,
        )
        .await
    }
}

#[test]
fn legacy_routes_load_as_codex_in_configuration_sessions_and_admissions() {
    let old_route = json!({"model":"legacy-model","effort":"medium"});
    let config: Config = serde_json::from_value(json!({"repair_route":old_route})).unwrap();
    assert_eq!(config.repair_route, Route::new("legacy-model", "medium"));
    assert_eq!(config.opencode_binary, "opencode");
    let session: Session = serde_json::from_value(json!({"id":"legacy-session","role":"repair","route":old_route,"status":"completed","started_at":"2026-09-09","summary":"kept"})).unwrap();
    assert_eq!(session.route.backend, Backend::Codex);
    let mut admission = serde_json::to_value(Admission::new(
        "cycle",
        None,
        "repair",
        &config.repair_route,
    ))
    .unwrap();
    admission["route"] = old_route;
    assert_eq!(
        serde_json::from_value::<Admission>(admission)
            .unwrap()
            .route
            .backend,
        Backend::Codex
    );
    let mut invalid = route();
    invalid.effort = "high".into();
    assert!(invalid.validate(false).is_err());
    let mut codex = Route::new("model", "high");
    codex.provider = Some("provider".into());
    assert!(codex.validate(false).is_err());
    assert!(serde_json::from_value::<Route>(json!({"backend":"unknown","model":"model"})).is_err());
}

#[tokio::test]
async fn native_catalog_is_safe_and_checks_provider_capabilities_and_variants() {
    let fixture = Fixture::new();
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    let models = client.models(&fixture.workspace).await.unwrap();
    let serialized = serde_json::to_string(&models).unwrap();
    assert!(
        !serialized.contains("secret")
            && !serialized.contains("credential")
            && !serialized.contains("PRIVATE_API_KEY")
    );
    validate_route(&route(), &models).unwrap();
    let mut selected = route();
    selected.provider = Some("alternate".into());
    validate_route(&selected, &models).unwrap();
    selected.provider = Some("offline".into());
    assert!(validate_route(&selected, &models).is_err());
    selected = route();
    selected.variant = Some("invented".into());
    assert!(validate_route(&selected, &models).is_err());
    selected.variant = None;
    selected.model = "plain-model".into();
    validate_route(&selected, &models).unwrap();
    selected.model = "no-tools".into();
    assert!(validate_route(&selected, &models).is_err());
}

#[tokio::test]
async fn native_sessions_survive_server_restarts_and_structured_reviews_are_validated() {
    let fixture = Fixture::new();
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    let session = client
        .start(&route(), &fixture.workspace, None)
        .await
        .unwrap();
    assert!(session.starts_with("ses_"));
    let answer = client
        .turn(
            &session,
            &route(),
            &fixture.workspace,
            "Fixture prompt",
            None,
        )
        .await
        .unwrap();
    assert_eq!(answer, "Fixture completed. ✓");
    drop(client);
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    assert_eq!(
        client
            .start(&route(), &fixture.workspace, Some(&session))
            .await
            .unwrap(),
        session
    );
    let fresh = client
        .start(&route(), &fixture.workspace, None)
        .await
        .unwrap();
    assert_ne!(fresh, session);
    let review = client
        .turn(
            &fresh,
            &route(),
            &fixture.workspace,
            "Fixture prompt",
            Some(schemas::review_schema()),
        )
        .await
        .unwrap();
    assert!(
        serde_json::from_str::<octomus_agent::model::Review>(&review)
            .unwrap()
            .clean()
    );
    let events = serde_json::to_string(&fixture.store.events(None).unwrap()).unwrap();
    assert!(!events.contains("private fixture"));
    fixture.mode("wrong-workspace");
    assert!(
        client
            .start(&route(), &fixture.workspace, Some(&session))
            .await
            .is_err()
    );
    std::fs::remove_file(fixture.temp.path().join("opencode-mode")).unwrap();
    std::fs::remove_file(
        fixture
            .temp
            .path()
            .join("oc-sessions")
            .join(format!("{session}.json")),
    )
    .unwrap();
    assert!(
        client
            .start(&route(), &fixture.workspace, Some(&session))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn native_failures_never_return_successful_evidence() {
    for mode in [
        "wrong-model",
        "wrong-variant",
        "wrong-session",
        "wrong-message",
        "incomplete",
        "truncated",
        "failed",
        "missing-structured",
        "malformed-structured",
        "interactive",
        "question",
        "interactive-v2",
        "question-v2",
        "disconnect",
        "events-disconnect",
        "invalid-event",
        "invalid-json",
        "oversized-json",
    ] {
        let fixture = Fixture::new();
        let client = fixture.connect(CancellationToken::new()).await.unwrap();
        let session = client
            .start(&route(), &fixture.workspace, None)
            .await
            .unwrap();
        fixture.mode(mode);
        let result = client
            .turn(
                &session,
                &route(),
                &fixture.workspace,
                "Fixture prompt",
                Some(schemas::review_schema()),
            )
            .await;
        assert!(result.is_err(), "{mode} unexpectedly succeeded: {result:?}");
        assert!(
            fixture.temp.path().join("opencode-aborts.jsonl").exists(),
            "{mode} was not aborted"
        );
        if ["interactive", "question", "interactive-v2", "question-v2"].contains(&mode) {
            assert!(fixture.temp.path().join("opencode-rejected").exists());
            assert!(
                result
                    .unwrap_err()
                    .to_string()
                    .contains("interactive input")
            );
        }
    }
}

fn alive(pid: u32) -> bool {
    std::fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|stat| !stat.contains(") Z"))
}
#[tokio::test]
async fn cancellation_stops_owned_server_and_descendants() {
    let fixture = Fixture::new();
    let cancel = CancellationToken::new();
    let client = fixture.connect(cancel.clone()).await.unwrap();
    let session = client
        .start(&route(), &fixture.workspace, None)
        .await
        .unwrap();
    fixture.mode("detached-hold");
    let selected = route();
    let turn = client.turn(
        &session,
        &selected,
        &fixture.workspace,
        "Fixture prompt",
        None,
    );
    let stop = async {
        tokio::time::timeout(Duration::from_secs(5), async {
            while !fixture.temp.path().join("opencode-child-pid").exists() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        cancel.cancel();
    };
    let (result, _) = tokio::join!(turn, stop);
    assert!(result.unwrap_err().to_string().contains("cancelled"));
    let pid: u32 = std::fs::read_to_string(fixture.temp.path().join("opencode-child-pid"))
        .unwrap()
        .parse()
        .unwrap();
    let server: Value = serde_json::from_str(
        std::fs::read_to_string(fixture.temp.path().join("opencode-pids.jsonl"))
            .unwrap()
            .lines()
            .next()
            .unwrap(),
    )
    .unwrap();
    drop(client);
    let stopped = tokio::time::timeout(Duration::from_secs(3), async {
        while alive(pid) || alive(server["pid"].as_u64().unwrap() as u32) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    // Clean up the fixture's own detached group even when this regression fails.
    if alive(pid) {
        unsafe {
            libc::kill(-(pid as i32), libc::SIGKILL);
        }
    }
    assert!(
        stopped.is_ok(),
        "Runner cleanup left a detached shell process alive"
    );
}

#[tokio::test]
async fn timeouts_and_startup_policy_failures_are_bounded() {
    for mode in ["startup-failure", "startup-hang", "wrong-policy"] {
        let fixture = Fixture::new();
        fixture.mode(mode);
        assert!(
            tokio::time::timeout(
                Duration::from_secs(4),
                fixture.connect(CancellationToken::new())
            )
            .await
            .unwrap()
            .is_err(),
            "{mode}"
        );
    }
    let fixture = Fixture::new();
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    let session = client
        .start(&route(), &fixture.workspace, None)
        .await
        .unwrap();
    fixture.mode("timeout");
    let error = tokio::time::timeout(
        Duration::from_secs(13),
        client.turn(
            &session,
            &route(),
            &fixture.workspace,
            "Fixture prompt",
            None,
        ),
    )
    .await
    .unwrap()
    .unwrap_err();
    assert!(format!("{error:#}").contains("time"));
    assert!(fixture.temp.path().join("opencode-aborts.jsonl").exists());
}

#[tokio::test]
async fn opencode_routes_do_not_require_codex_and_audits_skip_execution_runners() {
    let fixture = Fixture::new();
    let mut config = fixture.config.clone();
    for role in ["orchestrator", "discovery", "proposal_reviewer"] {
        config.roles.insert(role.into(), route());
    }
    let mut clients = Runners::new(
        &config,
        fixture.store.clone(),
        "fixture",
        CancellationToken::new(),
    );
    clients
        .validate_routes(&config, &fixture.workspace, true)
        .await
        .unwrap();
    assert!(
        clients
            .validate_routes(&config, &fixture.workspace, false)
            .await
            .is_err()
    );
    config.roles.insert("code_reviewer".into(), route());
    config.tiers.values_mut().for_each(|r| *r = route());
    config.repair_route = route();
    clients
        .validate_routes(&config, &fixture.workspace, false)
        .await
        .unwrap();
}

#[tokio::test]
#[ignore = "requires OCTOMUS_OPENCODE_SMOKE_BINARY pointing to the pinned CLI; no model calls"]
async fn pinned_opencode_protocol_smoke_without_model_calls() {
    let binary =
        std::env::var("OCTOMUS_OPENCODE_SMOKE_BINARY").expect("Set OCTOMUS_OPENCODE_SMOKE_BINARY");
    let mut fixture = Fixture::new();
    let config_path = fixture.temp.path().join("smoke.json");
    std::fs::write(&config_path, json!({"enabled_providers":["smoke"],"provider":{"smoke":{
        "npm":"@ai-sdk/openai-compatible","name":"Smoke provider","options":{"baseURL":"http://127.0.0.1:9/v1","apiKey":"unused-smoke-placeholder"},
        "models":{"smoke-model":{"name":"Smoke model","limit":{"context":8192,"output":1024},"tool_call":true,"variants":{"high":{"temperature":0.1}}}}
    }}}).to_string()).unwrap();
    let wrapper = &fixture.config.opencode_binary;
    // The CLI gets only temporary XDG directories, synthetic configuration, and adapter policy.
    std::fs::write(wrapper, format!("#!/usr/bin/env python3\nimport os, sys\nenv = {{k:v for k,v in os.environ.items() if k in ['PATH','LANG','OPENCODE_SERVER_USERNAME','OPENCODE_SERVER_PASSWORD','OPENCODE_CONFIG_CONTENT','OPENCODE_DISABLE_PROJECT_CONFIG','OPENCODE_DISABLE_AUTOUPDATE','OPENCODE_DISABLE_AUTOCOMPACT','OPENCODE_DISABLE_TERMINAL_TITLE']}}\nfor key in ['XDG_CONFIG_HOME','XDG_DATA_HOME','XDG_STATE_HOME','XDG_CACHE_HOME']:\n env[key] = {} + '/' + key\nenv['OPENCODE_CONFIG'] = {}\nbinary = {}\nos.execve(binary, [binary] + sys.argv[1:], env)\n", json!(fixture.temp.path().to_string_lossy()), json!(config_path), json!(binary))).unwrap();
    fixture.config.command_timeout_seconds = 60;
    fixture.config.session_timeout_seconds = 60;
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    assert_eq!(client.version(), octomus_agent::opencode::PROTOCOL_VERSION);
    let models = client.models(&fixture.workspace).await.unwrap();
    let selected = Route {
        provider: Some("smoke".into()),
        model: "smoke-model".into(),
        ..route()
    };
    validate_route(&selected, &models).unwrap();
    let session = client
        .start(&selected, &fixture.workspace, None)
        .await
        .unwrap();
    drop(client);
    let client = fixture.connect(CancellationToken::new()).await.unwrap();
    assert_eq!(
        client
            .start(&selected, &fixture.workspace, Some(&session))
            .await
            .unwrap(),
        session
    );
    let requests = fixture.store.events(None).unwrap();
    assert!(requests.is_empty(), "Smoke check must not start a turn");
}
