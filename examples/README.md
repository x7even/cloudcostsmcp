# OpenCloudCosts — Client Configuration Examples

Ready-to-copy config snippets for every major MCP client. All examples use
`uvx opencloudcosts` (requires [uv](https://docs.astral.sh/uv/)) which installs
and runs the latest published package automatically — no `git clone` needed.

**Public AWS and Azure pricing works with zero credentials.** Add credentials only
for the providers you need.

---

## Quick-start credential setup

Copy `.env.example` from the project root and set the values you need:

```bash
# AWS — public pricing (no creds), effective pricing needs Cost Explorer
export AWS_PROFILE=default
export OCC_AWS_ENABLE_COST_EXPLORER=true   # optional, costs $0.01/call

# GCP — free API key from console.cloud.google.com
export OCC_GCP_API_KEY=AIza...
# or Application Default Credentials: gcloud auth application-default login

# Azure — no credentials needed
```

Pass these via `env` in your client config or put them in a `.env` file and set
`envFile` where supported.

---

## Client configs

| Client | Transport | Config file | Notes |
|--------|-----------|------------|-------|
| [Claude Code](#claude-code) | stdio | `.mcp.json` or `~/.claude/settings.json` | Project or global scope |
| [Claude Desktop](#claude-desktop) | stdio | `claude_desktop_config.json` | Platform-specific path |
| [Claude.ai web](#claudeai-web) | HTTP | UI settings | Needs running HTTP server |
| [Cursor](#cursor) | stdio | `~/.cursor/mcp.json` | Global; also per-project `.cursor/mcp.json` |
| [Cline](#cline) | stdio | `cline_mcp_settings.json` | VS Code extension |
| [Windsurf](#windsurf) | stdio | `~/.codeium/windsurf/mcp_config.json` | |
| [Zed](#zed) | stdio | `~/.config/zed/settings.json` | Uses `context_servers` key |
| [Continue.dev](#continuedev) | stdio | `~/.continue/config.yaml` | Agent mode only |
| [Goose](#goose) | stdio | `~/.config/goose/config.yaml` | Block's open-source agent |
| [LM Studio](#lm-studio) | stdio | `~/.lmstudio/mcp.json` | v0.3.17+ |
| [Codex CLI](#codex-cli) | stdio | `~/.codex/config.toml` | OpenAI Codex CLI |
| [Ollama](#ollama) | — | via bridge | No native MCP support |
| [Any client](#http-transport) | HTTP | varies | Docker / remote server |

---

## Claude Code

**Project scope** — create `.mcp.json` in your repo root:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

**Global scope** — add to `~/.claude/settings.json`:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"]
    }
  }
}
```

→ Copy-paste ready: [`claude-code.json`](claude-code.json)

---

## Claude Desktop

macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`  
Windows: `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

→ Copy-paste ready: [`claude-desktop.json`](claude-desktop.json)

---

## Claude.ai web

Claude.ai web supports **remote MCP servers over HTTP**. You need a running
HTTP server — either Docker or a local instance:

```bash
# Local
uvx opencloudcosts --transport http --port 8080

# Docker
docker run -p 8080:8080 -e OCC_GCP_API_KEY=AIza... ghcr.io/x7even/opencloudcosts:latest
```

Then in Claude.ai → Settings → Integrations → Add MCP Server:
- **URL**: `http://localhost:8080/mcp/v1`
- **Name**: cloudcost

For remote servers (cloud VM, etc.) the URL must be HTTPS with a public address.

→ See: [`http-client.json`](http-client.json) for client-side HTTP config

---

## Cursor

Global: `~/.cursor/mcp.json`  
Per-project: `.cursor/mcp.json` in repo root

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

→ Copy-paste ready: [`cursor.json`](cursor.json)

---

## Cline

Cline stores MCP settings in VS Code's global storage. The easiest way is via
the Cline sidebar → MCP Servers → Add Server. Or edit the file directly:

macOS: `~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json`  
Windows: `%APPDATA%\Code\User\globalStorage\saoudrizwan.claude-dev\settings\cline_mcp_settings.json`  
Linux: `~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json`

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      },
      "disabled": false,
      "autoApprove": []
    }
  }
}
```

→ Copy-paste ready: [`cline.json`](cline.json)

---

## Windsurf

`~/.codeium/windsurf/mcp_config.json`

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

→ Copy-paste ready: [`windsurf.json`](windsurf.json)

---

## Zed

`~/.config/zed/settings.json` — merge the `context_servers` key into your existing settings:

```json
{
  "context_servers": {
    "cloudcost": {
      "command": {
        "path": "uvx",
        "args": ["opencloudcosts"],
        "env": {
          "AWS_PROFILE": "default",
          "OCC_GCP_API_KEY": "AIza..."
        }
      },
      "settings": {}
    }
  }
}
```

→ Copy-paste ready: [`zed.json`](zed.json)

---

## Continue.dev

`~/.continue/config.yaml` — merge `mcpServers` into your existing config.
MCP tools are available in **agent mode** only (not inline chat).

```yaml
mcpServers:
  - name: cloudcost
    command: uvx
    args:
      - opencloudcosts
    env:
      AWS_PROFILE: default
      OCC_GCP_API_KEY: "AIza..."
```

→ Copy-paste ready: [`continue.yaml`](continue.yaml)

---

## Goose

`~/.config/goose/config.yaml`

```yaml
extensions:
  cloudcost:
    name: cloudcost
    cmd: uvx
    args:
      - opencloudcosts
    enabled: true
    type: stdio
    timeout: 300
    envs:
      AWS_PROFILE: default
      OCC_GCP_API_KEY: "AIza..."
```

→ Copy-paste ready: [`goose.yaml`](goose.yaml)

---

## LM Studio

Requires LM Studio v0.3.17 or later.

`~/.lmstudio/mcp.json` (macOS/Linux)  
`%USERPROFILE%\.lmstudio\mcp.json` (Windows)

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

→ Copy-paste ready: [`lmstudio.json`](lmstudio.json)

---

## Codex CLI

`~/.codex/config.toml` (global) or `.codex/config.toml` (project, requires trust prompt)

```toml
[mcp_servers.cloudcost]
command = "uvx"
args    = ["opencloudcosts"]

[mcp_servers.cloudcost.env]
AWS_PROFILE    = "default"
OCC_GCP_API_KEY = "AIza..."
```

Or add via CLI:

```bash
codex mcp add cloudcost -- uvx opencloudcosts
```

→ Copy-paste ready: [`codex.toml`](codex.toml)

---

## Ollama

Ollama does not have native MCP support. Options:

**[mcphost](https://github.com/mark3labs/mcphost)** — lightweight Go CLI that bridges MCP servers to Ollama's API:

```bash
mcphost --model ollama/llama3.2 --config mcphost-config.json
```

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uvx",
      "args": ["opencloudcosts"]
    }
  }
}
```

**[Open WebUI](https://github.com/open-webui/open-webui)** — web frontend for Ollama with its own tool-calling layer. Configure OpenCloudCosts as an HTTP tool server running on `--transport http`.

---

## HTTP transport

For any client that supports remote MCP servers, or when running OpenCloudCosts
in Docker or on a remote host:

```bash
# Start the HTTP server
uvx opencloudcosts --transport http --port 8080

# Docker
docker run -p 8080:8080 \
  -e AWS_PROFILE=default \
  -e OCC_GCP_API_KEY=AIza... \
  ghcr.io/x7even/opencloudcosts:latest
```

Client config (for clients that support HTTP transport):

```json
{
  "mcpServers": {
    "cloudcost": {
      "transport": "http",
      "url": "http://localhost:8080/mcp/v1"
    }
  }
}
```

→ Copy-paste ready: [`http-client.json`](http-client.json)
