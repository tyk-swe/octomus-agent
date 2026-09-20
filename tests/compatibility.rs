//! Frozen M1 wire contracts, captured from the Rust reference. To refresh only
//! after reviewing a reference change: OCTOMUS_UPDATE_COMPAT=1 cargo test --test compatibility.
use octomus_agent::{config::*, engine::baseline::baseline_fingerprint, model::*};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

fn wire<T: DeserializeOwned + Serialize>(input: &str) -> Value {
    match serde_json::from_str::<T>(input) {
        Ok(value) => json!({"json":serde_json::to_string(&value).unwrap()}),
        Err(_) => json!({"rejected":true}),
    }
}
fn outcome(kind: &str, input: &str) -> Value {
    let mut value = match kind {
        "Route" => wire::<Route>(input),
        "Config" => wire::<Config>(input),
        "Proposal" => wire::<Proposal>(input),
        "PlanningCapacity" => wire::<PlanningCapacity>(input),
        "AttemptPolicy" => wire::<AttemptPolicy>(input),
        "WorkspaceLifecycle" => wire::<WorkspaceLifecycle>(input),
        "Finding" => wire::<Finding>(input),
        "Review" => wire::<Review>(input),
        "ReviewRound" => wire::<ReviewRound>(input),
        "Verification" => wire::<Verification>(input),
        "BaselineCommand" => wire::<BaselineCommand>(input),
        "BaselineCheck" => wire::<BaselineCheck>(input),
        "DefaultBranchObservation" => wire::<DefaultBranchObservation>(input),
        "Session" => wire::<Session>(input),
        "Task" => wire::<Task>(input),
        "PullRequest" => wire::<PullRequest>(input),
        "PrObservation" => wire::<PrObservation>(input),
        "Grounding" => wire::<Grounding>(input),
        "ExternalPrContext" => wire::<ExternalPrContext>(input),
        "PrCoverage" => wire::<PrCoverage>(input),
        "OpenPrInventory" => wire::<OpenPrInventory>(input),
        "PrCapacity" => wire::<PrCapacity>(input),
        "Cycle" => wire::<Cycle>(input),
        "RunBatch" => wire::<RunBatch>(input),
        "Control" => wire::<Control>(input),
        "Event" => wire::<Event>(input),
        "Backend" => wire::<Backend>(input),
        "Status" => wire::<Status>(input),
        "BlockedReason" => wire::<BlockedReason>(input),
        "PlanningCapacityStatus" => wire::<PlanningCapacityStatus>(input),
        "BaselineStatus" => wire::<BaselineStatus>(input),
        "CycleMode" => wire::<CycleMode>(input),
        "OperatingMode" => wire::<OperatingMode>(input),
        "BatchPhase" => wire::<BatchPhase>(input),
        _ => panic!("unknown fixture type {kind}"),
    };
    if value.get("rejected").is_some() {
        return value;
    }
    match kind {
        "Config" => {
            let c: Config = serde_json::from_str(input).unwrap();
            value["validation_error"] = json!(c.validate(false).err().map(|e| e.to_string()));
            value["fingerprint"] = json!(baseline_fingerprint(&c).unwrap());
        }
        "Route" => {
            let r: Route = serde_json::from_str(input).unwrap();
            value["validation_error"] = json!(r.validate(false).err().map(|e| e.to_string()));
            value["ready_error"] = json!(r.validate(true).err().map(|e| e.to_string()));
            value["display"] = json!(r.to_string());
        }
        "Task" => {
            let t: Task = serde_json::from_str(input).unwrap();
            value["actions"] = json!(t.allowed_actions());
            value["attempt_reviews"] = json!(t.attempt_reviews());
            value["execution_config"] =
                json!(serde_json::to_string(&t.execution_config()).unwrap());
        }
        "Review" => {
            let r: Review = serde_json::from_str(input).unwrap();
            value["clean"] = json!(r.clean());
            value["valid"] = json!(r.valid());
        }
        _ => {}
    }
    value
}
fn destination(raw: &str) -> Option<(String, String)> {
    // The reference parser is private; keep these checks verbatim with
    // src/notifications.rs::WebhookDestination::parse.
    let raw = raw.trim();
    if raw.is_empty() || raw.len() > 8192 {
        return None;
    }
    let url = reqwest::Url::parse(raw).ok()?;
    if !url.username().is_empty() || url.password().is_some() || url.fragment().is_some() {
        return None;
    }
    match url.scheme() {
        "https" => {
            url.host()?;
        }
        "http" => match url.host()? {
            url::Host::Ipv4(ip) if ip.is_loopback() => {}
            url::Host::Ipv6(ip) if ip.is_loopback() => {}
            _ => return None,
        },
        _ => return None,
    }
    Some((
        url.as_str().to_owned(),
        format!("{:x}", Sha256::digest(url.as_str().as_bytes())),
    ))
}
#[test]
fn frozen_m1_contracts() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/compatibility/m1.json");
    let mut corpus: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    let update = std::env::var_os("OCTOMUS_UPDATE_COMPAT").is_some();
    for case in corpus["cases"].as_array_mut().unwrap() {
        let actual = outcome(
            case["type"].as_str().unwrap(),
            case["input"].as_str().unwrap(),
        );
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual, "{}", case["name"]);
        }
    }
    for case in corpus["identities"].as_array_mut().unwrap() {
        let title = case["title"].as_str().unwrap();
        let key = case["key"].as_str().unwrap();
        let actual = json!(
            (if key.trim().is_empty() { title } else { key })
                .trim()
                .to_lowercase()
        );
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual);
        }
    }
    for case in corpus["destinations"].as_array_mut().unwrap() {
        let actual = json!(destination(case["raw"].as_str().unwrap()));
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual);
        }
    }
    for case in corpus["decision_memory"].as_array_mut().unwrap() {
        let actual = if case["paths"].as_array().unwrap().is_empty() {
            case["revision"].clone()
        } else {
            // git::git returns String::from_utf8_lossy(stdout).trim().to_owned().
            json!(format!(
                "{:x}",
                Sha256::digest(case["output"].as_str().unwrap().trim().as_bytes())
            ))
        };
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual);
        }
    }
    for case in corpus["repositories"].as_array_mut().unwrap() {
        let left: Config = serde_json::from_value(case["left"].clone()).unwrap();
        let right: Config = serde_json::from_value(case["right"].clone()).unwrap();
        let actual = json!(left.same_remote_identity(&right));
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual);
        }
    }
    for case in corpus["same_work"].as_array_mut().unwrap() {
        let left: Proposal = serde_json::from_value(case["left"].clone()).unwrap();
        let right: Proposal = serde_json::from_value(case["right"].clone()).unwrap();
        let actual = json!(left.same_work(&right));
        if update {
            case["expected"] = actual;
        } else {
            assert_eq!(case["expected"], actual);
        }
    }
    for case in corpus["structured"].as_array_mut().unwrap() {
        let schema = match case["kind"].as_str() {
            Some("proposal") => octomus_agent::schemas::proposal_schema(),
            Some("review") => octomus_agent::schemas::review_schema(),
            _ => case["schema"].clone(),
        };
        let actual = json!(
            octomus_agent::schemas::validate(&case["value"], &schema)
                .err()
                .map(|e| e.to_string())
        );
        if update {
            case["schema"] = schema;
            case["expected"] = actual;
        } else {
            assert_eq!(case["schema"], schema);
            assert_eq!(case["expected"], actual, "{}", case["name"]);
        }
    }
    if update {
        std::fs::write(
            path,
            format!("{}\n", serde_json::to_string_pretty(&corpus).unwrap()),
        )
        .unwrap();
    }
}
