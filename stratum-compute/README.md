# Stratum V1 AI Compute Payload Distributor (Nemotron PoUW)

## Overview

This module provides a reference implementation of the Stratum V1 protocol adapted for decentralized AI batch inference, high-performance compute (HPC), and Proof-of-Useful-Work (PoUW) task distribution.

By adopting the proven connection multiplexing and low-latency broadcast architecture of mining pools, this component allows GPU marketplace hosts to process batched compute and inference workloads during idle intervals between dedicated microVM rentals.

### The Proof of Useful Work (PoUW) Model
In traditional mining pools, dispatchers broadcast block header hashes. In this AI PoUW implementation:
1. **Cataloged Model Weight Verification**: The dispatcher and workers pin cryptographic SHA-256 digests of all model weights and tokenizer configurations via `internal/serving/catalog.go` (e.g. `nemotron-mini-4b` pinned to commit `791833e92ebddb0bc2c1007f6d2b6764f886a2ae`).
2. **Dynamic Workload Dispatch**: The pool broadcasts instruction prompts or token sequences (`mining.notify`) targeted at specific models with generation constraints (`max_tokens`, `temperature`, `top_p`).
3. **Execution & Attestation**: Connected workers execute inference on local GPU hardware (via vLLM or local model runners) and submit verified execution proofs, token counts, and latency attestations back to the pool (`mining.submit`).

## Architecture

```
┌──────────────────────────────────────────────┐
│        Stratum Pool Dispatcher               │
│           (Python asyncio)                   │
│   - mining.subscribe (model capability reg)  │
│   - mining.notify (Nemotron task dispatch)   │
│   - mining.submit (verified share accounting)│
└──────────────────────┬───────────────────────┘
                       │ TCP (port 3333)
           ┌───────────┴───────────┐
           ▼                       ▼
    ┌──────────────┐        ┌──────────────┐
    │ Rust Worker  │        │ Rust Worker  │
    │  (Node A)    │        │  (Node B)    │
    │  GPU / vLLM  │        │  GPU / vLLM  │
    └──────────────┘        └──────────────┘
```

- **Pool Dispatcher (`pool/stratum_pool.py`)**: Asynchronous TCP server handling worker registration, model routing, job broadcasting, and submission validation.
- **Compute Worker (`worker/src/main.rs`)**: High-performance Rust client using `tokio` for non-blocking network I/O, model capability negotiation, and inference processing.
- **Serving Catalog (`internal/serving/catalog.go`)**: Upstream-integrated model registry with pinned revisions, exact byte counts, and SHA-256 verification.

## Supported Models

### `nemotron-mini-4b`
- **Source**: `nvidia/Nemotron-Mini-4B-Instruct`
- **Pinned Revision**: `791833e92ebddb0bc2c1007f6d2b6764f886a2ae`
- **Engine**: `vllm` (PagedAttention, continuous batching)
- **Minimum VRAM**: 8 GB (16 GB recommended for full context)
- **Total Weights**: `8,404,151,211` bytes (~7.82 GiB) across 8 verified files.

## Getting Started

### Prerequisites
- Python 3.10+
- Rust 1.75+ (Cargo)
- Go 1.22+

### Running the Pool
```bash
# Start pool listening on port 3333 with default model nemotron-mini-4b
python pool/stratum_pool.py --port 3333 --model nemotron-mini-4b
```

### Running the Worker
```bash
cd worker
cargo run --release
```
Environment variables:
- `STRATUM_POOL_ADDR`: Pool address (default: `127.0.0.1:3333`)
- `WORKER_NAME`: Worker identifier (default: `worker-01`)
- `MODEL_NAME`: Target model to serve (default: `nemotron-mini-4b`)
- `ARTIFACT_HASH`: Verified model artifact root hash
- `VRAM_GB`: Available host VRAM in GB (default: `16`)

### Testing Job Distribution
Run the example feeder script to dispatch Nemotron instruction workloads:
```bash
python pool/feeder_example.py
```

