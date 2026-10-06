#!/usr/bin/env python3
"""
Stratum V1 AI Compute Pool Dispatcher
Manages persistent TCP worker connections and broadcasts compute payloads.
"""

import asyncio
import json
import logging
from typing import Set

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("StratumPool")


class StratumPool:
    def __init__(self, host: str = "0.0.0.0", port: int = 3333):
        self.host = host
        self.port = port
        self.clients: Set[asyncio.StreamWriter] = set()
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
                    response = {
                        "id": msg_id,
                        "result": ["subscribed", "Extranonce1"],
                        "error": None,
                    }
                    writer.write((json.dumps(response) + "\n").encode("utf-8"))
                    await writer.drain()

                elif method == "mining.submit":
                    params = message.get("params", [])
                    worker_name = params[0] if len(params) > 0 else "unknown"
                    job_id = params[1] if len(params) > 1 else "unknown"
                    result = params[2] if len(params) > 2 else None

                    logger.info(f"Submission received from {worker_name} for job {job_id}: {result}")
                    response = {"id": msg_id, "result": True, "error": None}
                    writer.write((json.dumps(response) + "\n").encode("utf-8"))
                    await writer.drain()

        except ConnectionResetError:
            pass
        except Exception as e:
            logger.error(f"Error handling worker {addr}: {e}")
        finally:
            self.clients.discard(writer)
            writer.close()
            try:
                await writer.wait_closed()
            except Exception:
                pass
            logger.info(f"Worker {addr} disconnected.")

    async def broadcast_job(self, payload: list):
        """
        Broadcasts a compute payload to all currently connected workers.
        """
        if not self.clients:
            logger.warning("No workers connected to receive broadcast.")
            return

        self.job_counter += 1
        job_msg = {
            "id": None,
            "method": "mining.notify",
            "params": [
                str(self.job_counter),
                payload,
                True,  # clean_jobs flag
            ],
        }
        encoded = (json.dumps(job_msg) + "\n").encode("utf-8")

        for client in list(self.clients):
            try:
                client.write(encoded)
                await client.drain()
            except Exception:
                self.clients.discard(client)

    async def start(self):
        server = await asyncio.start_server(self.handle_client, self.host, self.port)
        logger.info(f"Stratum AI Pool listening on {self.host}:{self.port}")
        async with server:
            await server.serve_forever()


if __name__ == "__main__":
    pool = StratumPool()
    try:
        asyncio.run(pool.start())
    except KeyboardInterrupt:
        logger.info("Pool stopped by operator.")
