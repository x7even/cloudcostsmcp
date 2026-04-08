# T29 · HTTP/SSE transport support

**Status:** pending  
**Branch:** task/T29-http-transport

## Overview
Currently only stdio MCP transport is supported. HTTP+SSE enables:
- Shared team deployment (one server, many clients)
- Docker/container deployments
- Remote access without installing the package locally

## Files to change
- `src/opencloudcosts/server.py` — add `--transport` / `--port` CLI args
- `src/opencloudcosts/config.py` — add `http_port`, `http_host`, `api_key` fields
- `Dockerfile` (new) — containerised deployment
- `docker-compose.yml` (new, optional) — example compose file
- `README.md` — HTTP server and Docker sections

## Implementation

### 1. CLI args (server.py)
Check how FastMCP exposes transport configuration. FastMCP may support:
```python
mcp.run(transport="streamable-http", host="0.0.0.0", port=8080)
```
or may require a different startup path. Investigate FastMCP docs first.

Add CLI argument parsing to the entry point:
```python
import argparse

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--transport", choices=["stdio", "http"], default="stdio")
    parser.add_argument("--host", default=os.getenv("OCC_HTTP_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.getenv("OCC_HTTP_PORT", "8080")))
    args = parser.parse_args()
    
    if args.transport == "http":
        mcp.run(transport="streamable-http", host=args.host, port=args.port)
    else:
        mcp.run()
```

### 2. Config additions (config.py)
```python
http_port: int = Field(default=8080, description="HTTP server port")
http_host: str = Field(default="127.0.0.1", description="HTTP bind address")
api_key: str = Field(default="", description="Bearer token for HTTP transport auth")
```

### 3. Optional auth middleware
If `OCC_API_KEY` is set, add a middleware that checks `Authorization: Bearer {key}` on incoming requests. FastMCP may support this natively or require a custom wrapper.

```python
if settings.api_key:
    @mcp.middleware
    async def auth_check(request, call_next):
        auth = request.headers.get("Authorization", "")
        if auth != f"Bearer {settings.api_key}":
            return {"error": "Unauthorized"}, 401
        return await call_next(request)
```

### 4. Dockerfile
```dockerfile
FROM python:3.12-slim

RUN pip install uv

WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --no-dev

COPY src/ src/

ENV OCC_HTTP_HOST=0.0.0.0
ENV OCC_HTTP_PORT=8080

EXPOSE 8080

CMD ["uv", "run", "opencloudcosts", "--transport", "http", "--host", "0.0.0.0"]
```

### 5. docker-compose.yml (optional)
```yaml
version: "3.8"
services:
  opencloudcosts:
    build: .
    ports:
      - "8080:8080"
    environment:
      - OCC_HTTP_HOST=0.0.0.0
      - OCC_HTTP_PORT=8080
      - OCC_API_KEY=${OCC_API_KEY:-}
      - AWS_PROFILE=${AWS_PROFILE:-default}
      - OCC_GCP_API_KEY=${OCC_GCP_API_KEY:-}
    volumes:
      - ~/.aws:/root/.aws:ro
      - ~/.cache/opencloudcosts:/root/.cache/opencloudcosts
```

### 6. .mcp.json example for HTTP transport
```json
{
  "mcpServers": {
    "cloudcost": {
      "transport": "http",
      "url": "http://localhost:8080/mcp/v1",
      "headers": {"Authorization": "Bearer your-api-key"}
    }
  }
}
```

## README additions
Two new sections:

### Running as HTTP server
```bash
uv run opencloudcosts --transport http --port 8080
```

### Docker
```bash
docker build -t opencloudcosts .
docker run -p 8080:8080 \
  -e OCC_API_KEY=your-secret \
  -v ~/.aws:/root/.aws:ro \
  opencloudcosts
```

## Verification
- `stdio` mode must still work unchanged after this change
- `http` mode: test with `curl http://localhost:8080/mcp/v1` (or MCP Inspector pointed at HTTP URL)
- Auth: request without `Authorization` header returns 401 when `OCC_API_KEY` set
- Request with correct token succeeds

## Acceptance criteria
- `uv run opencloudcosts` (no args) still works as stdio
- `uv run opencloudcosts --transport http` starts HTTP server on port 8080
- `--port` and `--host` flags work
- `OCC_API_KEY` enables bearer token auth on HTTP mode
- Dockerfile builds and runs correctly
- README has clear setup instructions for both modes
