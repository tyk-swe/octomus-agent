//! Structured outputs shared by all agent runners.
use anyhow::{Result, bail, ensure};
use serde_json::{Value, json};

/// Validate the small schema vocabulary used by Octomus before treating native output as evidence.
pub fn validate(value: &Value, schema: &Value) -> Result<()> {
    match schema["type"].as_str() {
        Some("object") => {
            let object = value
                .as_object()
                .ok_or_else(|| anyhow::anyhow!("Structured result must be an object"))?;
            let properties = schema["properties"]
                .as_object()
                .ok_or_else(|| anyhow::anyhow!("Invalid object schema"))?;
            if let Some(required) = schema["required"].as_array() {
                for key in required {
                    ensure!(
                        key.as_str().is_some_and(|key| object.contains_key(key)),
                        "Structured result is missing required field {key}"
                    );
                }
            }
            for (key, value) in object {
                if let Some(schema) = properties.get(key) {
                    validate(value, schema)?;
                } else {
                    ensure!(
                        schema["additionalProperties"] != false,
                        "Structured result has an unexpected field"
                    );
                }
            }
        }
        Some("array") => {
            let values = value
                .as_array()
                .ok_or_else(|| anyhow::anyhow!("Structured result must be an array"))?;
            for value in values {
                validate(value, &schema["items"])?;
            }
        }
        Some("string") => ensure!(value.is_string(), "Structured result must be a string"),
        Some("boolean") => ensure!(value.is_boolean(), "Structured result must be a boolean"),
        _ => bail!("Unsupported structured result schema"),
    }
    Ok(())
}

pub fn object(properties: Value) -> Value {
    let required = properties
        .as_object()
        .unwrap()
        .keys()
        .cloned()
        .collect::<Vec<_>>();
    json!({"type":"object","properties":properties,"required":required,"additionalProperties":false})
}
pub fn string() -> Value {
    json!({"type":"string"})
}
pub fn array(items: Value) -> Value {
    json!({"type":"array","items":items})
}
pub fn proposal_schema() -> Value {
    object(
        json!({"proposals":array(object(json!({"id":string(),"title":string(),"problem":string(),"evidence":array(string()),"benefit":string(),"category":string(),"target":string(),"tier":string(),"scope":string(),"dependencies":array(string()),"prompt":string(),"decision":string(),"reason":string(),"problem_key":string(),"relevant_paths":array(string()),"reconsiders":array(string())})))}),
    )
}
pub fn review_schema() -> Value {
    object(
        json!({"completed":{"type":"boolean"},"summary":string(),"findings":array(object(json!({"title":string(),"file":string(),"detail":string(),"priority":string()})))}),
    )
}
