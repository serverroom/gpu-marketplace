#!/usr/bin/env python3
"""
Example Workload Feeder for Stratum AI Pool
Dispatches Nemotron inference workloads to test worker connectivity and PoUW throughput.
"""

import asyncio
import logging
from stratum_pool import StratumPool

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("WorkloadFeeder")


async def feeder(pool: StratumPool):
    logger.info("Waiting for workers to connect...")
    while not pool.clients:
        await asyncio.sleep(1)

    logger.info("Worker detected. Starting Nemotron inference workload dispatch...")

    # Real instruction & reasoning prompts targeting Nemotron-Mini-4B
    sample_jobs = [
        {
            "prompt": "Analyze GPU memory bandwidth saturation across 8x H100 SXM5 nodes with NVLink 4.",
            "generation": {"max_tokens": 128, "temperature": 0.2}
        },
        {
            "prompt": "Explain the architectural difference between PagedAttention and standard multi-head attention in vLLM.",
            "generation": {"max_tokens": 128, "temperature": 0.5}
        },
        {
            "prompt": "Write a high-performance Rust asynchronous TCP frame decoder using tokio and bytes::BytesMut.",
            "generation": {"max_tokens": 160, "temperature": 0.1}
        },
    ]

    for idx, job in enumerate(sample_jobs):
        logger.info(f"Broadcasting Nemotron Job #{idx + 1}: '{job['prompt'][:60]}...'")
        await pool.broadcast_job(
            payload=job["prompt"],
            model="nemotron-mini-4b",
            generation_params=job["generation"]
        )
        await asyncio.sleep(3)

    logger.info("Nemotron sample dispatch complete.")


async def main():
    pool = StratumPool(host="127.0.0.1", port=3333, default_model="nemotron-mini-4b")
    server_task = asyncio.create_task(pool.start())
    feeder_task = asyncio.create_task(feeder(pool))
    await asyncio.gather(server_task, feeder_task)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass

