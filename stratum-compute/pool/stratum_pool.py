#!/usr/bin/env python3
"""
Stratum V1 AI Compute Pool Dispatcher
Manages persistent TCP worker connections and broadcasts AI inference workloads (Nemotron PoUW).
"""

import asyncio
import json
import logging
from typing import Dict, Optional, Set

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("StratumPool")


class StratumPool:
    def __init__(self, host: str = "0.0.0.0", port: int = 3333, default_model: str = "nemotron-mini-4b"):
        self.host = host
        self.port = port
        self.default_model = default_model
        self.clients: Set[asyncio.StreamWriter] = set()
        self.worker_capabilities: Dict[asyncio.StreamWriter, dict] = {}
        self.active_jobs: Dict[str, dict] = {}
        self.job_counter = 0

    async def handle_client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
        addr = writer.get_extra_info("peername")
        logger.info(f"Worker connected from {addr}")
        self.clients.add(writer)

        try:
            while True:
                data = await reader.readline()
                if not data:
                    break

                try:
                    message = json.loads(data.decode("utf-8").strip())
                except (json.JSONDecodeError, UnicodeDecodeError):
                    continue

                method = message.get("method")
                msg_id = message.get("id")

                if method == "mining.subscribe":
                    params = message.get("params", [])
                    worker_name = params[0] if len(params) > 0 else f"worker-{addr[1]}"
                    caps = params[1] if len(params) > 1 and isinstance(params[1], dict) else {}

                    self.worker_capabilities[writer] = {
                        "name": worker_name,
                        "addr": addr,
                        "models": caps.get("models", [self.default_model]),
                        "artifact_hash": caps.get("artifact_hash", ""),
                        "vram_gb": caps.get("vram_gb", 16),
                    }

                    logger.info(f"Worker '{worker_name}' subscribed with models: {self.worker_capabilities[writer]['models']}")

                    response = {
                        "id": msg_id,
                        "result": [
                            "subscribed",
                            "Extranonce1",
                            {"active_model": self.default_model, "session_id": str(addr[1])}
                        ],
                        "error": None,
                    }
                    writer.write((json.dumps(response) + "\n").encode("utf-8"))
                    await writer.drain()

                elif method == "mining.submit":
                    params = message.get("params", [])
                    worker_name = params[0] if len(params) > 0 else "unknown"
                    job_id = params[1] if len(params) > 1 else "unknown"
                    result = params[2] if len(params) > 2 else None
                    attestation = params[3] if len(params) > 3 and isinstance(params[3], dict) else {}

                    job_meta = self.active_jobs.get(str(job_id), {})
                    model = job_meta.get("model", self.default_model)

                    logger.info(
                        f"Inference share received from {worker_name} for Job #{job_id} [{model}]: "
                        f"tokens={attestation.get('token_count', 'N/A')}, "
                        f"latency_ms={attestation.get('latency_ms', 'N/A')}"
                    )

                    response = {
                        "id": msg_id,
                        "result": True,
                        "error": None,
                        "verified": True
                    }
                    writer.write((json.dumps(response) + "\n").encode("utf-8"))
                    await writer.drain()

        except ConnectionResetError:
            pass
        except Exception as e:
            logger.error(f"Error handling worker {addr}: {e}")
        finally:
            self.clients.discard(writer)
            self.worker_capabilities.pop(writer, None)
            writer.close()
            try:
                await writer.wait_closed()
            except Exception:
                pass
            logger.info(f"Worker {addr} disconnected.")

    async def broadcast_job(
        self,
        payload,
        model: Optional[str] = None,
        generation_params: Optional[dict] = None
    ):
        """
        Broadcasts an AI inference workload to all connected workers capable of serving the model.
        """
        if not self.clients:
            logger.warning("No workers connected to receive broadcast.")
            return

        target_model = model or self.default_model
        self.job_counter += 1
        job_id = str(self.job_counter)

        gen_config = generation_params or {
            "max_tokens": 256,
            "temperature": 0.7,
            "top_p": 0.9,
            "stop": ["<|endoftext|>", "<|eot_id|>"]
        }

        # Register in active jobs ledger
        self.active_jobs[job_id] = {
            "model": target_model,
            "payload": payload,
            "generation": gen_config,
        }

        job_msg = {
            "id": None,
            "method": "mining.notify",
            "params": [
                job_id,
                target_model,
                payload,
                gen_config,
                True,  # clean_jobs flag
            ],
        }
        encoded = (json.dumps(job_msg) + "\n").encode("utf-8")

        dispatched_count = 0
        for client in list(self.clients):
            caps = self.worker_capabilities.get(client, {})
            if target_model in caps.get("models", [target_model]):
                try:
                    client.write(encoded)
                    await client.drain()
                    dispatched_count += 1
                except Exception:
                    self.clients.discard(client)
                    self.worker_capabilities.pop(client, None)

        logger.info(f"Broadcasted Job #{job_id} [{target_model}] to {dispatched_count} worker(s).")

    async def start(self):
        server = await asyncio.start_server(self.handle_client, self.host, self.port)
        logger.info(f"Stratum AI Pool listening on {self.host}:{self.port} (Model: {self.default_model})")
        async with server:
            await server.serve_forever()


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Stratum V1 AI Compute Pool")
    parser.add_argument("--host", default="0.0.0.0", help="Listen host (default: 0.0.0.0)")
    parser.add_argument("--port", type=int, default=3333, help="Listen port (default: 3333)")
    parser.add_argument("--model", default="nemotron-mini-4b", help="Default target model (default: nemotron-mini-4b)")
    args = parser.parse_args()

    pool = StratumPool(host=args.host, port=args.port, default_model=args.model)
    try:
        asyncio.run(pool.start())
    except KeyboardInterrupt:
        logger.info("Pool stopped by operator.")

