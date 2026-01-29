"""
SmolFaaS Example Python Function

A simple HTTP function demonstrating Python runtime support with FastAPI.
"""

import os
import socket
from datetime import datetime, timezone

import uvicorn
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI(
    title="SmolFaaS Python Function",
    description="Example Python function for SmolFaaS",
    version="1.0.0"
)


class Response(BaseModel):
    message: str
    runtime: str
    timestamp: str
    hostname: str


@app.get("/", response_model=Response)
def root():
    """Root endpoint returning function info."""
    return Response(
        message="Hello from SmolFaaS Python function!",
        runtime="python",
        timestamp=datetime.now(timezone.utc).isoformat(),
        hostname=socket.gethostname()
    )


@app.get("/health")
def health():
    """Health check endpoint."""
    return {"status": "OK"}


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8000))
    print(f"Python function starting on port {port}...")
    uvicorn.run(app, host="0.0.0.0", port=port)
