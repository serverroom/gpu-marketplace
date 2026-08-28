import asyncio
import glob
import os
import logging
from stratum_pool import StratumPool

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(name)s - %(levelname)s - %(message)s')
logger = logging.getLogger("TicketFeeder")

async def ticket_feeder(pool):
    logger.info("Waiting for GPU/CPU workers to connect before dispatching tickets...")
    while not pool.clients:
        await asyncio.sleep(1)
    
    logger.info("Worker detected. Initializing Ticket Ingestion...")
    
    artifact_dir = r"C:\Users\theal\.gemini\antigravity\brain\e775956b-f12a-46ef-b03e-d7a39dcf14ea"
    
    # Grab all ticket artifacts
    files = glob.glob(os.path.join(artifact_dir, "*.md"))
    tickets = [f for f in files if "ticket" in f.lower()]
    
    for idx, t_path in enumerate(set(tickets)):
        with open(t_path, 'r', encoding='utf-8') as f:
            content = f.read()
        
        # Convert text to UTF-8 integer array (simulating Tokenization for the AI model)
        # This allows the Stratum protocol to remain strictly mathematical
        tokens = list(content.encode('utf-8'))
        
        filename = os.path.basename(t_path)
        logger.info(f"Dispatching Workload [{filename}] -> {len(tokens)} tokens")
        
        # Emit job to the grid
        await pool.broadcast_job(tokens)
        
        # Wait 3 seconds before sending the next ticket to simulate network flow
        await asyncio.sleep(3)

async def main():
    # Bind locally for the test
    pool = StratumPool(host='127.0.0.1', port=3334)
    
    server_task = asyncio.create_task(pool.start())
    feeder_task = asyncio.create_task(ticket_feeder(pool))
    
    await asyncio.gather(server_task, feeder_task)

if __name__ == "__main__":
    asyncio.run(main())
