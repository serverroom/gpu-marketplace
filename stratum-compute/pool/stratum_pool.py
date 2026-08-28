import asyncio
import json
import logging

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(name)s - %(levelname)s - %(message)s')
logger = logging.getLogger("StratumPool")

class StratumPool:
    def __init__(self, host='0.0.0.0', port=3333):
        self.host = host
        self.port = port
        self.clients = set()
        self.job_counter = 0

    async def handle_client(self, reader, writer):
        addr = writer.get_extra_info('peername')
        logger.info(f"Worker connected from {addr}")
        self.clients.add(writer)

        try:
            while True:
                data = await reader.readline()
                if not data:
                    break
                
                try:
                    message = json.loads(data.decode().strip())
                except json.JSONDecodeError:
                    continue

                if message.get('method') == 'mining.subscribe':
                    response = {"id": message.get('id'), "result": ["subscribed", "Extranonce1"], "error": None}
                    writer.write((json.dumps(response) + "\n").encode())
                    await writer.drain()
                
                elif message.get('method') == 'mining.submit':
                    worker_name = message.get('params', [])[0] if message.get('params') else "Unknown"
                    job_id = message.get('params', [])[1] if len(message.get('params', [])) > 1 else "Unknown"
                    
                    logger.info(f"Solution submitted by Worker {worker_name} for Job {job_id}")
                    # In a production environment, validation logic against the payload would execute here.
                    response = {"id": message.get('id'), "result": True, "error": None}
                    writer.write((json.dumps(response) + "\n").encode())
                    await writer.drain()

        except Exception as e:
            logger.error(f"Error handling client {addr}: {e}")
        finally:
            self.clients.remove(writer)
            writer.close()
            logger.info(f"Worker {addr} disconnected.")

    async def broadcast_job(self, payload_vector: list):
        """
        Public method to be called by the upstream data ingestion service.
        Broadcasts the compute payload to all connected GPU workers.
        """
        if not self.clients:
            return
            
        self.job_counter += 1
        job_payload = {
            "id": None,
            "method": "mining.notify",
            "params": [
                str(self.job_counter),
                payload_vector,
                True # Clean jobs flag
            ]
        }
        
        payload_str = json.dumps(job_payload) + "\n"
        
        # Gather all writes to handle network I/O concurrently
        await asyncio.gather(*(self._write_to_client(c, payload_str) for c in list(self.clients)))

    async def _write_to_client(self, client, data: str):
        try:
            client.write(data.encode())
            await client.drain()
        except Exception:
            pass # Disconnects are handled by the reader loop

    async def start(self):
        server = await asyncio.start_server(self.handle_client, self.host, self.port)
        logger.info(f"Stratum AI Pool listening on {self.host}:{self.port}")
        
        async with server:
            await server.serve_forever()

if __name__ == '__main__':
    pool = StratumPool()
    # In production, broadcast_job() would be triggered by an external message queue (e.g., Kafka/Redis)
    asyncio.run(pool.start())
