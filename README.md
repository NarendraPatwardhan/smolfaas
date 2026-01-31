# SmolFaaS

A lightweight Function-as-a-Service (FaaS) platform for local development and deployment.

## Overview

SmolFaaS is a simple yet powerful FaaS platform that uses Docker, BuildKit, and Caddy to build and deploy serverless functions locally. Beyond being a functional tool, SmolFaaS is designed as a **learning resource** - the entire implementation lives in a single, well-commented `main.go` file that can be read top-to-bottom to understand how a FaaS system works.

## Features

- **Multi-Runtime Support**: Go, Python, Node.js, and custom Dockerfile
- **Incremental Builds**: Content-based hashing (SQLite) for efficient rebuilds
- **LLB-based Building**: Uses BuildKit's Low-Level Builder for optimized container builds
- **Dynamic Routing**: Caddy reverse proxy with automatic route configuration
- **Consistency Checking**: Verify state of functions, containers, and routes
- **Single-File Architecture**: Educational design inspired by SQLite

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         SmolFaaS CLI                            │
│                     (Cobra-based CLI)                           │
└─────────────────┬───────────────────────────────────────────────┘
                  │
    ┌─────────────┼─────────────┬─────────────────┐
    ▼             ▼             ▼                 ▼
┌────────┐  ┌───────────┐  ┌──────────┐  ┌─────────────┐
│BuildKit│  │  FaaS     │  │ Function │  │   Caddy     │
│Manager │  │  Builder  │  │ Manager  │  │   Manager   │
└────┬───┘  └─────┬─────┘  └────┬─────┘  └──────┬──────┘
     │            │             │               │
     ▼            ▼             ▼               ▼
┌────────┐  ┌───────────┐  ┌──────────┐  ┌─────────────┐
│BuildKit│  │ Runtime   │  │ Function │  │   Caddy     │
│Daemon  │  │ Handlers  │  │Containers│  │ Container   │
└────────┘  └───────────┘  └──────────┘  └─────────────┘
```

## Requirements

- Docker Engine
- Go 1.24+ (with CGO enabled for SQLite)

## Installation

```bash
# Clone and build
git clone <repository>
cd smolfaas
make build

# Or install to GOPATH/bin
make install
```

## Quick Start

1. **Create a functions directory** with your function code:

```
functions/
├── my-go-func/
│   ├── go.mod
│   ├── go.sum
│   └── main.go
└── my-python-func/
    ├── pyproject.toml
    └── main.py
```

2. **Start SmolFaaS**:

```bash
smolfaas up
```

3. **Invoke your functions**:

```bash
curl -Lk http://fn.localhost/invoke/my-go-func
curl -Lk http://fn.localhost/invoke/my-python-func
```

4. **Stop SmolFaaS**:

```bash
smolfaas down
```

## Testing Functions with curl

Once SmolFaaS is running, you can test your functions using curl. The router listens on `fn.localhost` by default (ports 80 and 443).

**Note**: Caddy automatically enables HTTPS and redirects HTTP to HTTPS. Use `-L` to follow redirects and `-k` to accept the self-signed certificate for local testing.

### Test the Example Functions

```bash
# Test Go function
curl -Lk http://fn.localhost/invoke/example-go-func
# Expected: {"message":"Hello from SmolFaaS Go function!","runtime":"go","timestamp":"...","hostname":"..."}

# Test Node.js function
curl -Lk http://fn.localhost/invoke/example-node-func
# Expected: {"message":"Hello from SmolFaaS Node.js function!","runtime":"node","timestamp":"...","hostname":"..."}

# Test Python function
curl -Lk http://fn.localhost/invoke/example-py-func
# Expected: {"message":"Hello from SmolFaaS Python function!","runtime":"python","timestamp":"...","hostname":"..."}

# Test router health endpoint
curl -Lk http://fn.localhost/health
# Expected: SmolFaaS Router OK
```

### Test with Headers and JSON

```bash
# POST request with JSON body
curl -Lk -X POST http://fn.localhost/invoke/my-func \
  -H "Content-Type: application/json" \
  -d '{"name": "world"}'

# Check response headers
curl -Lki http://fn.localhost/invoke/example-go-func

# Pretty print JSON response (requires jq)
curl -sLk http://fn.localhost/invoke/example-go-func | jq .
```

### Using Custom Hostname

If you specified a custom hostname with `-a`:

```bash
smolfaas up -a myapp.local
curl -Lk http://myapp.local/invoke/example-go-func
```

## Commands

### `smolfaas up`

Discovers functions, builds container images via BuildKit, starts function containers, and configures Caddy routing.

```bash
smolfaas up [flags]

Flags:
  -d, --functions-dir string   Directory containing function sources (default "./functions")
  -a, --addr string            Router hostname (default "fn.localhost")
  -n, --network string         Docker network name (default "smolfaas")
  -p, --image-prefix string    Image/container name prefix (default "smolfaas-func")
```

### `smolfaas down`

Stops and removes SmolFaaS services.

```bash
# Stop all services
smolfaas down

# Stop specific functions
smolfaas down my-go-func my-python-func

# Full cleanup (removes volumes, network, state)
smolfaas down --force
```

### `smolfaas check`

Verifies consistency of functions, containers, and routes. Reports-only, no automatic fixes.

```bash
smolfaas check
```

Output example:
```
Consistency Check Results:
==========================

my-go-func:
  ✓ Source hash matches stored state
  ✓ Container running
  ✓ Image matches stored state

my-python-func:
  ✗ Source hash changed (rebuild needed)
  ✓ Container running
  ✓ Image matches stored state

Summary: 1 function(s) need attention. Run 'smolfaas up' to fix.
```

## Supported Runtimes

### Go

**Detection**: `go.mod` file present

**Build Process**:
1. Download dependencies (cached separately)
2. Build static binary with `-ldflags='-s -w' -trimpath`
3. Copy to minimal Alpine runtime

**Function Requirements**:
- HTTP server listening on port 8000
- Exported as static binary

### Python

**Detection**: `pyproject.toml` or `requirements.txt` present

**Build Process**:
1. Install uv package manager
2. Install dependencies (cached separately)
3. Copy source code

**Function Requirements**:
- HTTP server (FastAPI, Flask, etc.) listening on port 8000
- Entry point: `main.py`

### Node.js

**Detection**: `package.json` present

**Build Process**:
1. Detect package manager (npm, yarn, pnpm)
2. Install dependencies (cached via lock files)
3. Run build script if present
4. Copy to runtime

**Function Requirements**:
- HTTP server listening on port 8000
- Entry point: `index.js`

### Dockerfile (Fallback)

**Detection**: `Dockerfile` present (highest priority)

When a `Dockerfile` is present in the function directory, SmolFaaS uses it instead of the built-in runtime handlers.

## Configuration

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--functions-dir, -d` | `./functions` | Directory containing function sources |
| `--addr, -a` | `fn.localhost` | Router hostname |
| `--network, -n` | `smolfaas` | Docker network name |
| `--caddy-name` | `smolfaas-router` | Caddy container name |
| `--buildkit-name` | `smolfaas-buildkitd` | BuildKit container name |
| `--image-prefix, -p` | `smolfaas-func` | Image/container name prefix |
| `--state-db` | `.smolfaas` | SQLite state database path |

## State Management

SmolFaaS uses SQLite to track function state:

- **Content Hash**: SHA256 of all source files
- **Image ID**: Docker image ID
- **Last Build Time**: Timestamp of last successful build

This enables efficient incremental builds - functions are only rebuilt when their source content changes.

## Example Functions

### Go Function

```go
// functions/hello-go/main.go
package main

import (
    "fmt"
    "net/http"
)

func main() {
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintln(w, "Hello from Go!")
    })
    http.ListenAndServe(":8000", nil)
}
```

### Python Function

```python
# functions/hello-python/main.py
import uvicorn
from fastapi import FastAPI

app = FastAPI()

@app.get("/")
def hello():
    return {"message": "Hello from Python!"}

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8000)
```

### Node.js Function

```javascript
// functions/hello-node/index.js
const http = require('http');

const server = http.createServer((req, res) => {
    res.writeHead(200, {'Content-Type': 'text/plain'});
    res.end('Hello from Node.js!\n');
});

server.listen(8000, () => {
    console.log('Server running on port 8000');
});
```

## Design Philosophy

SmolFaaS follows the SQLite philosophy where the codebase serves dual purposes:

1. **A production-ready tool** that solves real problems
2. **An educational resource** that teaches system design

The entire implementation lives in a single `main.go` file with comprehensive comments explaining:
- How container orchestration works
- How BuildKit and LLB operate
- How reverse proxies route traffic dynamically
- How to compose these into a cohesive platform

## License

MIT License - See LICENSE file for details.
