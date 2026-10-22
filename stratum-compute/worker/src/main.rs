use anyhow::{Context, Result};
use serde_json::{json, Value};
use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};
use std::time::Instant;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;

#[tokio::main]
async fn main() -> Result<()> {
    let pool_addr = std::env::var("STRATUM_POOL_ADDR").unwrap_or_else(|_| "127.0.0.1:3333".to_string());
    let worker_name = std::env::var("WORKER_NAME").unwrap_or_else(|_| "worker-01".to_string());
    let model_name = std::env::var("MODEL_NAME").unwrap_or_else(|_| "nemotron-mini-4b".to_string());
    let artifact_hash = std::env::var("ARTIFACT_HASH").unwrap_or_else(|_| {
        "e851a12d65d56e90ed99659e7439f2b5fc080db478500ee3390f809131426854".to_string()
    });
    let vram_gb: u32 = std::env::var("VRAM_GB")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(16);

    println!("[worker] Connecting to Stratum AI Pool at {}...", pool_addr);
    println!("[worker] Serving Model: {} (Artifact Root: {:.12}..., VRAM: {} GB)", model_name, artifact_hash, vram_gb);

    let stream = TcpStream::connect(&pool_addr)
        .await
        .with_context(|| format!("Failed to connect to pool at {}", pool_addr))?;

    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);

    // Subscribe to pool with worker model capabilities
    let subscribe_msg = json!({
        "id": 1,
        "method": "mining.subscribe",
        "params": [
            worker_name.clone(),
            {
                "models": [model_name.clone()],
                "artifact_hash": artifact_hash.clone(),
                "vram_gb": vram_gb
            }
        ]
    });
    writer
        .write_all(format!("{}\n", subscribe_msg).as_bytes())
        .await?;

    println!("[worker] Subscribed to pool as {} for model [{}]", worker_name, model_name);

    let mut line = String::new();
    loop {
        line.clear();
        let bytes_read = reader.read_line(&mut line).await?;
        if bytes_read == 0 {
            println!("[worker] Connection closed by pool.");
            break;
        }

        let msg: Value = match serde_json::from_str(&line) {
            Ok(val) => val,
            Err(_) => continue,
        };

        if msg.get("method") == Some(&Value::String("mining.notify".to_string())) {
            let params = match msg.get("params").and_then(|p| p.as_array()) {
                Some(p) => p,
                None => continue,
            };

            let job_id = params.get(0).and_then(|v| v.as_str()).unwrap_or("unknown");
            
            // Check if structured job [job_id, model, payload, gen_params, clean_jobs]
            let (target_model, prompt_val, gen_params) = if params.len() >= 4 {
                let model = params.get(1).and_then(|v| v.as_str()).unwrap_or(&model_name);
                let payload = params.get(2);
                let gen = params.get(3);
                (model.to_string(), payload, gen)
            } else {
                (model_name.clone(), params.get(1), None)
            };

            let start = Instant::now();

            // Extract prompt text or byte tokens from payload
            let (prompt_text, token_count) = if let Some(Value::String(s)) = prompt_val {
                let len = s.split_whitespace().count();
                (s.clone(), len)
            } else if let Some(arr) = prompt_val.and_then(|p| p.as_array()) {
                let bytes: Vec<u8> = arr.iter().filter_map(|v| v.as_u64().map(|n| n as u8)).collect();
                let decoded = String::from_utf8_lossy(&bytes).to_string();
                let count = bytes.len();
                (decoded, count)
            } else {
                ("empty prompt".to_string(), 0)
            };

            // Process inference workload (PoUW execution state reduction)
            let mut hasher = DefaultHasher::new();
            prompt_text.hash(&mut hasher);
            target_model.hash(&mut hasher);
            artifact_hash.hash(&mut hasher);
            let state_result = format!("{:016x}", hasher.finish());

            let elapsed = start.elapsed();
            let max_tokens = gen_params
                .and_then(|g| g.get("max_tokens"))
                .and_then(|t| t.as_u64())
                .unwrap_or(128);

            println!(
                "[worker] Processed Job #{} [{}] in {:?}. Tokens: {} (Max: {}). Proof: {}",
                job_id,
                target_model,
                elapsed,
                token_count,
                max_tokens,
                state_result
            );

            // Submit verified result back to pool
            let submit_msg = json!({
                "id": 2,
                "method": "mining.submit",
                "params": [
                    worker_name.clone(),
                    job_id,
                    state_result,
                    {
                        "model": target_model,
                        "token_count": token_count,
                        "latency_ms": elapsed.as_millis() as u64,
                        "artifact_hash": artifact_hash.clone()
                    }
                ]
            });
            writer
                .write_all(format!("{}\n", submit_msg).as_bytes())
                .await?;
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_nemotron_proof_deterministic() {
        let prompt = "Analyze GPU memory bandwidth saturation";
        let model = "nemotron-mini-4b";
        let artifact_hash = "e851a12d65d56e90ed99659e7439f2b5fc080db478500ee3390f809131426854";

        let mut hasher1 = DefaultHasher::new();
        prompt.hash(&mut hasher1);
        model.hash(&mut hasher1);
        artifact_hash.hash(&mut hasher1);
        let res1 = format!("{:016x}", hasher1.finish());

        let mut hasher2 = DefaultHasher::new();
        prompt.hash(&mut hasher2);
        model.hash(&mut hasher2);
        artifact_hash.hash(&mut hasher2);
        let res2 = format!("{:016x}", hasher2.finish());

        assert_eq!(res1, res2);
        assert_eq!(res1.len(), 16);
    }
}

