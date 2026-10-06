#!/usr/bin/env python3
"""
Example Workload Feeder for Stratum AI Pool
Dispatches sample matrix/tensor workloads to test worker connectivity.
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

    logger.info("Worker detected. Starting sample workload dispatch...")

    # Sample batch payloads (tokenized or quantized integer representations)
    sample_jobs = [
        [ord(c) for c in "Batch 1: Compute matrix multiply 1024x1024 fp16"],
        [ord(c) for c in "Batch 2: Generate embedding vector for query"],
        [ord(c) for c in "Batch 3: Evaluate attention head weights layer 12"],
    ]

    for idx, payload in enumerate(sample_jobs):
        logger.info(f"Broadcasting job {idx + 1} ({len(payload)} bytes)...")
        await pool.broadcast_job(payload)
        await asyncio.sleep(2)

    logger.info("Sample dispatch complete.")


async def main():
    pool = StratumPool(host="127.0.0.1", port=3333)
    server_task = asyncio.create_task(pool.start())
    feeder_task = asyncio.create_task(feeder(pool))
    await asyncio.gather(server_task, feeder_task)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
