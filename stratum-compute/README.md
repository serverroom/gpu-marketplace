# Stratum V1 AI Compute Payload Distributor

## Overview

This module provides a reference implementation of the Stratum V1 protocol adapted for decentralized AI batch inference and high-performance compute (HPC) task distribution.

By adopting the proven connection multiplexing and low-latency broadcast architecture of mining pools, this component allows GPU marketplace hosts to process batched compute workloads during idle intervals between dedicated microVM rentals.

### The Proof of Useful Work (PoUW) Model
In traditional mining pools, dispatchers broadcast block header hashes. In this PoUW implementation, the dispatcher broadcasts an N-dimensional state vector or tokenized tensor payload. Connected workers perform neural network inference, embedding calculations, or matrix operations on local hardware and return the resulting state hash or tensor output as their submission.

## Architecture

```
┌──────────────────────────────────────┐
│        Stratum Pool Dispatcher       │
│           (Python asyncio)           │
│   - mining.subscribe                 │
│   - mining.notify (broadcast jobs)   │
│   - mining.submit (verify solutions) │
└──────────────────┬───────────────────┘
                   │ TCP (port 3333)
       ┌───────────┴───────────┐
       ▼                       ▼
┌──────────────┐        ┌──────────────┐
│ Rust Worker  │        │ Rust Worker  │
│  (Node A)    │        │  (Node B)    │
│  GPU / CPU   │        │  GPU / CPU   │
└──────────────┘        └──────────────┘
```

- **Pool Dispatcher (`pool/stratum_pool.py`)**: Asynchronous TCP server handling worker registration, state broadcasts, and submission validation.
- **Compute Worker (`worker/src/main.rs`)**: High-performance Rust client using `tokio` for non-blocking network I/O and payload processing.

## Getting Started

### Prerequisites
- Python 3.10+
- Rust 1.75+ (Cargo)

### Running the Pool
```bash
python pool/stratum_pool.py
```
By default, the pool listens on `0.0.0.0:3333`.

### Running the Worker
```bash
cd worker
cargo run --release
```
Environment variables:
- `STRATUM_POOL_ADDR`: Pool address (default: `127.0.0.1:3333`)
- `WORKER_NAME`: Worker identifier (default: `worker-01`)

### Testing Job Distribution
Run the example feeder script to dispatch sample workloads to all connected workers:
```bash
python pool/feeder_example.py
```
