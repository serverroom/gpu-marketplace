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

    println!("[worker] Connecting to Stratum AI Pool at {}...", pool_addr);

    let stream = TcpStream::connect(&pool_addr)
        .await
        .with_context(|| format!("Failed to connect to pool at {}", pool_addr))?;

    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);

    // Subscribe to pool
    let subscribe_msg = json!({
        "id": 1,
        "method": "mining.subscribe",
        "params": [worker_name.clone()]
    });
    writer
        .write_all(format!("{}\n", subscribe_msg).as_bytes())
        .await?;

    println!("[worker] Subscribed to pool as {}", worker_name);

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
            let payload_data = params.get(1);

            let start = Instant::now();

            // Extract byte tokens from the broadcasted payload vector
            let tokens: Vec<u8> = payload_data
                .and_then(|p| p.as_array())
                .map(|arr| {
                    arr.iter()
                        .filter_map(|v| v.as_u64().map(|n| n as u8))
                        .collect()
                })
                .unwrap_or_default();

            // Execute compute work (demonstration: state vector reduction via hashing)
            let mut hasher = DefaultHasher::new();
            tokens.hash(&mut hasher);
            let state_result = format!("{:016x}", hasher.finish());

            let elapsed = start.elapsed();
            println!(
                "[worker] Processed Job {} ({} bytes) in {:?}. Result: {}",
                job_id,
                tokens.len(),
                elapsed,
                state_result
            );

            // Submit verified result back to pool
            let submit_msg = json!({
                "id": 2,
                "method": "mining.submit",
                "params": [worker_name.clone(), job_id, state_result]
            });
            writer
                .write_all(format!("{}\n", submit_msg).as_bytes())
                .await?;
        }
    }

    Ok(())
}
