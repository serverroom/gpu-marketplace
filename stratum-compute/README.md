# ServerRoom GPU Grid - Stratum V1 AI Payload Distributor

## Overview
This repository provides a production-grade implementation of the Stratum V1 protocol, custom-engineered for decentralized AI inference and high-performance computing (HPC) task distribution. 

By leveraging the battle-tested architecture of cryptocurrency mining pools, this software allows infrastructure providers to distribute generalized compute payloads (such as neural network states, matrix operations, or combinatorial optimizations) across an arbitrary grid of GPU/NPU workers with sub-millisecond latency.

### The Proof of Useful Work (PoUW) Paradigm
Traditional mining pools distribute block headers for SHA-256 hashing. This software replaces the block header payload with an N-dimensional state vector (representing a live AI environment or compute state). Workers perform tensor inference instead of hashing, returning the computed logits or equilibrium state as the "hash solution."

## Architecture

* **The Pool (Python `asyncio`)**: Acts as the centralized dispatcher. It ingests live, external data streams (e.g., market data, LLM requests, physics simulations) and broadcasts the serialized tensor states to all connected workers via the `mining.notify` Stratum RPC method.
* **The Worker (Rust `tokio`)**: A bare-metal compute client built in Rust. It maintains a persistent TCP connection to the pool, parses incoming state payloads, and routes the data to local GPU/CPU hardware for execution. It returns the computed solution via `mining.submit`.

## Production Hardening
* **Zero-Trust Network Tolerance**: Designed to operate cleanly across SSH tunnels or VPN meshes (e.g., Wireguard, Tailscale) to bypass strict datacenter ingress firewalls.
* **Non-Blocking Dispatch**: The pool utilizes pure `asyncio` to handle thousands of concurrent TCP connections without blocking payload generation.
* **Memory-Safe Worker**: The worker is written in safe Rust using `tokio`, ensuring zero memory leaks during prolonged, high-frequency execution.

## Deployment Guidelines
* Ensure `tokio` threads are scaled to match the physical core count of the worker nodes.
* For AI payloads, integrate the worker's byte-stream directly into your chosen tensor library (e.g., LibTorch, CUDA, or ONNX Runtime) via FFI. 
* Do not expose the Pool TCP port (`3333`) to the public internet without standard DDoS mitigation (e.g., HAProxy / TCP Shield).
