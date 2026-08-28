use tokio::net::TcpStream;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use serde_json::{Value, json};
use std::time::Instant;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // Configuration
    let pool_ip = std::env::var("STRATUM_POOL_IP").unwrap_or_else(|_| "127.0.0.1:3334".to_string());
    let worker_name = std::env::var("WORKER_NAME").unwrap_or_else(|_| "node-01".to_string());

    println!("[WORKER] Initializing compute environment...");
    println!("[WORKER] Connecting to AI Stratum Pool at {}...", pool_ip);
    
    let mut stream = TcpStream::connect(&pool_ip).await?;
    let (reader, mut writer) = stream.split();
    let mut reader = BufReader::new(reader);

    // Subscribe to pool
    let sub_req = json!({"id": 1, "method": "mining.subscribe", "params": []});
    writer.write_all(format!("{}\n", sub_req).as_bytes()).await?;
    
    let mut line = String::new();
    loop {
        line.clear();
        let bytes_read = reader.read_line(&mut line).await?;
        if bytes_read == 0 {
            println!("[WORKER] Connection closed by pool.");
            break;
        }

        let msg: Value = match serde_json::from_str(&line) {
            Ok(m) => m,
            Err(_) => continue,
        };
        
        if msg["method"] == "mining.notify" {
            let job_id = msg["params"][0].as_str().unwrap_or("unknown");
            let payload_vector = &msg["params"][1];
            
            let start = Instant::now();
            
            // --- TICKET INGESTION & "EMBEDDING" ---
            // 1. Read the tokenized UTF-8 array from the Stratum Payload
            let tokens: Vec<u8> = payload_vector.as_array().unwrap_or(&vec![])
                .iter()
                .filter_map(|v| v.as_u64().map(|n| n as u8))
                .collect();
                
            // 2. Decode it back to text for the console output
            let text = String::from_utf8_lossy(&tokens);
            let preview = if text.len() > 120 { &text[0..120] } else { &text };
            
            println!("\n[WORKER] 📥 Received Ticket Workload Job {} ({} tokens)", job_id, tokens.len());
            println!("[WORKER] 📄 Content Preview: {}...", preview.replace('\n', " "));
            
            // 3. Perform Work (Simulated Embedding via Hashing)
            use std::collections::hash_map::DefaultHasher;
            use std::hash::{Hash, Hasher};
            let mut hasher = DefaultHasher::new();
            tokens.hash(&mut hasher);
            let result_hash = format!("{:016x}", hasher.finish());
            
            let duration = start.elapsed();
            println!("[WORKER] 🧠 Embedding complete in {:?}. Vector Hash: {}", duration, result_hash);
            
            // Submit the answer back to the Stratum Pool
            let sub_res = json!({
                "id": 2, 
                "method": "mining.submit", 
                "params": [worker_name.clone(), job_id, result_hash]
            });
            
            writer.write_all(format!("{}\n", sub_res).as_bytes()).await?;
        }
    }
    
    Ok(())
}
