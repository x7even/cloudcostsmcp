#!/usr/bin/env python3
"""
LLM Test Harness for OpenCloudCosts MCP

Runs test prompts through any OpenAI-compatible LLM API with the full
MCP tool-call agentic loop. Results are saved per-run for analysis.

Configuration (copy .env.example to .env and edit):
    OCC_LLM_BASE_URL   Base URL of your LLM server, no /v1 suffix (e.g. http://127.0.0.1:8080)
    OCC_LLM_MODEL      Model identifier as reported by the server
    OCC_LLM_API_KEY    Optional API key (for OpenAI, hosted providers, etc.)
    OCC_MCP_URL        HTTP MCP endpoint — uses containerised server instead of stdio subprocess
                       (e.g. http://your-mcp-host:8080/mcp). Unset = spawn local stdio process.

Usage:
    uv run local-test-harness/run_tests.py
    uv run local-test-harness/run_tests.py --ids C1,C4,X2
    uv run local-test-harness/run_tests.py --ids all --parallel 8
    uv run local-test-harness/run_tests.py --mcp-url http://your-mcp-host:8080/mcp --parallel 8
"""
# /// script
# requires-python = ">=3.11"
# dependencies = ["httpx", "mcp"]
# ///

import argparse
import asyncio
import json
import os
import re
import sys
from datetime import datetime
from pathlib import Path

import httpx
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client
from mcp.client.streamable_http import streamablehttp_client

# ---------------------------------------------------------------------------
# Configuration — read from .env file then environment variables.
# CLI flags (--llm-base-url, --model, --api-key) override everything.
# ---------------------------------------------------------------------------
_HARNESS_DIR = Path(__file__).parent


def _load_dotenv(env_file: Path) -> None:
    """Load key=value pairs from a .env file (does not override existing vars)."""
    if not env_file.exists():
        return
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key and key not in os.environ:
            os.environ[key] = value


_load_dotenv(_HARNESS_DIR / ".env")

LLM_BASE_URL = os.environ.get("OCC_LLM_BASE_URL", "")
LLM_MODEL = os.environ.get("OCC_LLM_MODEL", "")
LLM_API_KEY = os.environ.get("OCC_LLM_API_KEY", "")  # optional
MCP_URL = os.environ.get("OCC_MCP_URL", "")  # HTTP MCP endpoint; if unset, falls back to stdio
PROJECT_DIR = _HARNESS_DIR.parent
MCP_COMMAND = "uv"
MCP_ARGS = ["run", "--directory", str(PROJECT_DIR), "opencloudcosts"]
# Absolute safety cap — only fires if loop detection fails to catch a pathological case.
MAX_TOOL_ROUNDS = 150
MAX_TOOL_RESULT_CHARS = 6000  # Prevent context overflow from large tool responses
# Sliding window for loop detection: if the same (tool, args) fingerprint appears
# twice within this many consecutive calls, the model is in a loop.
LOOP_DETECT_WINDOW = 6
RESULTS_DIR = _HARNESS_DIR / "results"

SYSTEM_PROMPT = (
    "You are a cloud cost analyst. Use the available tools to look up real pricing data. "
    "Always call tools to get actual prices — never estimate or recall figures from memory. "
    "When a tool response includes a 'see_also', 'next_steps', 'not_included', or "
    "'not_included_action' field, follow those hints by making the suggested additional "
    "tool calls before answering. "
    "Be concise and structured in your final response. "
    "If some data is unavailable (e.g. a provider requires credentials you don't have), "
    "still provide a final answer covering what you were able to retrieve, and clearly "
    "note which items are missing and why. "
    "IMPORTANT: You MUST always write your final answer as regular text in your response. "
    "Do not leave your response blank — always end with a clear written answer. "
    "\n\nCRITICAL GROUNDING RULES — you will be penalised for violating these:\n"
    "1. NEVER substitute training-data prices when a tool returns empty results or an error. "
    "If a tool returns no pricing for an item, state 'pricing unavailable from tool' for that item — do not invent a figure.\n"
    "2. NEVER override or 'correct' a price returned by a tool, even if it looks wrong to you. "
    "Report tool results exactly; note discrepancies in a comment but keep the tool figure.\n"
    "3. For multi-cloud comparisons, call tools for EACH provider separately before answering. "
    "If a provider returns no data, write 'Unable to retrieve [provider] pricing' — never fill it in from memory.\n"
    "4. If estimate_bom returns a 'not_included' list, call get_price for each listed item individually. "
    "Do not estimate or guess any cost that was not returned by a tool.\n\n"
    "TOOL CALL FORMAT — always use this exact format when calling a tool:\n"
    "<tool_call>\n"
    '{"name": "TOOL_NAME", "arguments": {"param": "value"}}\n'
    "</tool_call>"
)

# ---------------------------------------------------------------------------
# Test prompts
# ---------------------------------------------------------------------------
TEST_PROMPTS = {
    # --- Common ---
    "C1": (
        "What does an m5.2xlarge cost on-demand in us-east-1? "
        "Give me the hourly and monthly figure."
    ),
    "C2": ("I need 2TB of gp3 EBS storage in us-west-2. What will that cost per month?"),
    "C3": (
        "I want to run a c6g.4xlarge on Linux. Which AWS region is cheapest, "
        "and how much cheaper is it than us-east-1?"
    ),
    "C4": (
        "Compare on-demand, 1-year No Upfront, and 1-year All Upfront pricing for "
        "an r6i.xlarge in eu-west-1. What's the monthly saving if I commit for a year?"
    ),
    # --- Multi-cloud ---
    "M1": (
        "Compare the price of roughly 8 vCPU / 32GB RAM compute instances across "
        "AWS, GCP, and Azure in their respective US East regions. Which is cheapest per hour?"
    ),
    "M2": (
        "What does 500GB of premium/SSD block storage cost per month on AWS (gp3), "
        "GCP (pd-ssd), and Azure (premium-ssd) in US East regions?"
    ),
    "M3": (
        "Estimate the monthly cost of this stack on both AWS (us-east-1) and Azure (eastus): "
        "2x web servers (4 vCPU / 16GB), 1x database server (8 vCPU / 32GB), 500GB block storage. "
        "Which cloud is cheaper and by how much?"
    ),
    # --- Complex ---
    "X1": (
        "I'm planning a production deployment on AWS us-east-1: 3x m5.xlarge app servers, "
        "1x db.r6g.large MySQL RDS (Single-AZ), 500GB gp3 EBS. Give me a full monthly TCO "
        "including anything else I should be pricing — don't just give me compute and storage."
    ),
    "X2": (
        "I have a fleet of 5x m5.4xlarge in us-east-1 running 24/7. Walk me through the "
        "1-year reserved instance options (No Upfront, Partial Upfront, All Upfront) and "
        "tell me which saves the most over 12 months compared to on-demand."
    ),
    # -----------------------------------------------------------------------
    # AWS Simple (AS)
    # -----------------------------------------------------------------------
    "AS1": "What does an r6i.2xlarge cost on-demand (Linux) in ap-southeast-1? Give hourly and monthly.",
    "AS2": "Which AWS region is cheapest for a p3.2xlarge GPU instance? Compare against us-east-1.",
    "AS3": "I need 1TB of io2 EBS storage provisioned at 5000 IOPS in us-west-2. What's the monthly cost?",
    "AS4": "What does a db.t4g.medium MySQL RDS instance (Single-AZ) cost per month in us-east-1?",
    "AS5": "Compare spot vs on-demand pricing for a t3.xlarge in us-east-1. What's the discount percentage?",
    "AS6": "List the available m7g (Graviton3) instance types in eu-west-1 and show their on-demand prices.",
    "AS7": (
        "What's the price difference between a c6i.4xlarge and a c6a.4xlarge in us-east-1? "
        "Which is cheaper and by how much per month?"
    ),
    "AS8": "How much does AWS charge for data transfer from us-east-1 to eu-west-1?",
    "AS9": "What does a NAT Gateway cost in us-east-1? Give me the hourly charge and the per-GB data processing fee.",
    "AS10": "What's the on-demand price for an ElastiCache cache.r7g.large (Redis) node in us-east-1?",
    # -----------------------------------------------------------------------
    # GCP Simple (GS)
    # -----------------------------------------------------------------------
    "GS1": "What does an n2-standard-4 cost on-demand in us-central1? Give hourly and monthly.",
    "GS2": "Which GCP region is cheapest for an e2-standard-8 instance?",
    "GS3": "What does 500GB of pd-ssd storage cost per month in europe-west1?",
    "GS4": "Compare n2-standard-8 vs e2-standard-8 on-demand pricing in us-east1. Which is cheaper?",
    "GS5": "What's the 1-year committed use discount (CUD) price for a c2-standard-8 in us-central1?",
    "GS6": (
        "Show a sample of GCP compute instances in asia-southeast1 with at least 16 vCPUs and their on-demand Linux prices. "
        "Make exactly one list_instance_types call (it returns up to 25 results — treat those as the complete sample, "
        "do not attempt to retrieve more instances). Then price the returned instances with get_prices_batch. "
        "Once you have the prices, present the results in a table and stop — do not make any further tool calls."
    ),
    "GS7": "What does an n2d-standard-4 cost on-demand in us-central1?",
    "GS8": "What's the on-demand price for a c3-standard-8 in us-east4?",
    "GS9": "Compare pd-balanced vs pd-ssd storage costs for 1TB in us-central1.",
    "GS10": "What's the on-demand price for an a2-highgpu-1g (GPU) instance in us-central1?",
    # -----------------------------------------------------------------------
    # Multi-product AWS vs GCP comparisons (MP)
    # -----------------------------------------------------------------------
    "MP1": (
        "Compare the total monthly cost of running 4 vCPU / 16GB compute plus 500GB SSD block storage "
        "on AWS (us-east-1) vs GCP (us-central1). Use real instance types for each — pick the closest match."
    ),
    "MP2": (
        "I'm choosing between AWS and GCP for a two-tier stack: 2x general-purpose 4 vCPU / 16GB web servers "
        "and 1x 8 vCPU / 32GB database server. Compare total monthly cost on both clouds in their US East regions."
    ),
    "MP3": (
        "Compare the cost of running a GPU workload for 100 hours on AWS (p3.2xlarge) vs GCP (a2-highgpu-1g) "
        "in US regions. Which is cheaper and by how much?"
    ),
    "MP4": (
        "Compare 1-year reserved/committed pricing for an 8 vCPU / 32GB instance on AWS (us-east-1, reserved_1yr) "
        "vs GCP (us-central1, cud_1yr). Which offers a bigger discount off on-demand?"
    ),
    "MP5": (
        "What's cheaper for 2TB of block storage — AWS gp3 in us-east-1 or GCP pd-balanced in us-central1? "
        "Show the monthly cost and per-GB rate for each."
    ),
    # -----------------------------------------------------------------------
    # Multi-region comparisons within AWS & GCP (MR)
    # -----------------------------------------------------------------------
    "MR1": (
        "Compare the on-demand price of an m5.2xlarge across three AWS regions: "
        "us-east-1, eu-west-1, and ap-southeast-1. Which is cheapest and what's the % difference?"
    ),
    "MR2": (
        "Compare n2-standard-8 on-demand pricing across three GCP regions: "
        "us-central1, europe-west1, and asia-east1. Which is cheapest?"
    ),
    "MR3": "Find the cheapest AWS region for a c6g.2xlarge across all major regions and show the top 5.",
    "MR4": "Find the cheapest GCP region for an e2-standard-4 across all major regions.",
    "MR5": (
        "I'm considering migrating my r6i.xlarge workload from us-east-1 to eu-west-1. "
        "Compare the compute cost in both regions. If I transfer 1TB of data per month during migration, "
        "what's the data egress cost from us-east-1?"
    ),
    # -----------------------------------------------------------------------
    # Complex BoM / TCO (CX)
    # -----------------------------------------------------------------------
    "CX1": (
        "Give me a full monthly TCO for a 3-tier web application on AWS us-east-1: "
        "3x m5.xlarge web servers, 2x m5.2xlarge app servers, 1x db.r6g.xlarge MySQL RDS (Multi-AZ), "
        "500GB gp3 EBS. Include any additional costs I should know about."
    ),
    "CX2": (
        "Estimate the monthly cost of a data processing pipeline on AWS us-east-1: "
        "4x c6i.2xlarge compute nodes, 2TB gp3 EBS for working storage. "
        "What's the full bill including anything else typically needed?"
    ),
    "CX3": (
        "I have a fleet of 10x m5.xlarge in us-east-1 running 24/7. "
        "Compare on-demand vs 1-year reserved (No Upfront) vs 3-year reserved (No Upfront). "
        "Show total 12-month and 36-month costs for each option."
    ),
    "CX4": (
        "Estimate the monthly cost of this GCP stack in us-central1: "
        "4x n2-standard-4 web servers, 2x n2-standard-8 database servers, 1TB pd-ssd storage."
    ),
    "CX5": (
        "I'm running 2x m5.large instances in us-east-1 to handle 5 million requests per month. "
        "What is my infrastructure cost per 1,000 requests?"
    ),
    "CX6": (
        "Full TCO for a Kubernetes cluster on AWS us-east-1: "
        "6x m5.2xlarge worker nodes, 3x r6i.xlarge stateful workload nodes, 1TB gp3 EBS. "
        "What should I budget monthly including supporting services?"
    ),
    "CX7": (
        "I have 5x r6i.xlarge instances in us-east-1 running 24/7. "
        "Compare total 12-month cost: on-demand vs 1-year Partial Upfront reserved. "
        "What is my annual saving in dollars and percent?"
    ),
    "CX8": (
        "Estimate monthly cost for a machine learning platform on AWS us-east-1: "
        "2x p3.2xlarge for GPU training (run 200 hours/month each), "
        "4x c6i.xlarge for CPU inference (always-on), 2TB gp3 EBS."
    ),
    "CX9": (
        "I need to run the same stack in two AWS regions for redundancy: "
        "2x m5.xlarge + 500GB gp3 EBS in us-east-1 AND eu-west-1. "
        "What is the total combined monthly cost? Are the regions priced differently?"
    ),
    "CX10": (
        "Unit economics question: I run 3x m5.xlarge app servers and 1x db.r6g.large MySQL RDS (Single-AZ) "
        "in us-east-1 to serve 50,000 monthly active users. "
        "What is my infrastructure cost per user per month?"
    ),
    "CX_BOM": (
        "Give me a full AWS Bill of Materials table for this production platform in us-east-1. "
        "Look up every line item and present the results as a markdown table with columns: "
        "Service | Type/SKU | Qty | Unit Price | Monthly Cost. "
        "Then add a TOTAL row. Here is the full inventory:\n"
        "COMPUTE:\n"
        "  - 6x m5.xlarge (Linux, on-demand) — web tier\n"
        "  - 4x m5.2xlarge (Linux, on-demand) — API tier\n"
        "  - 2x c6i.4xlarge (Linux, on-demand) — batch processing\n"
        "  - 2x r6g.2xlarge (Linux, on-demand) — in-memory analytics\n"
        "  - 1x p3.2xlarge (Linux, on-demand) — ML inference\n"
        "  - 3x t3.medium (Linux, on-demand) — bastion / management\n"
        "  - 2x m5.4xlarge (Linux, 1-year reserved No Upfront) — reporting\n"
        "DATABASE:\n"
        "  - 1x db.r6g.2xlarge MySQL RDS Multi-AZ\n"
        "  - 1x db.t4g.medium PostgreSQL RDS Single-AZ (dev)\n"
        "  - 1x cache.r7g.large ElastiCache Redis node\n"
        "  - DynamoDB on-demand: 50M read request units + 10M write request units/month\n"
        "STORAGE:\n"
        "  - 2TB gp3 EBS (across fleet)\n"
        "  - 500GB io1 EBS at 3000 IOPS (for RDS data volume)\n"
        "NETWORK:\n"
        "  - 1x Application Load Balancer\n"
        "  - 2x NAT Gateways processing 500GB/month each\n"
        "  - CloudFront CDN serving 10TB/month egress\n"
        "SERVERLESS / OTHER:\n"
        "  - AWS Lambda: 10 million requests/month\n"
        "  - CloudWatch: 500GB log ingestion/month\n"
    ),
    "CX11": (
        "Monthly cost for an AWS media processing platform in us-west-2: "
        "4x c6i.2xlarge transcoding servers, 2x r6i.xlarge caching nodes, "
        "10TB st1 EBS for media storage, 2TB gp3 EBS for working storage. "
        "Give me a full itemised bill."
    ),
    "CX12": (
        "Give me the monthly bill for a data warehouse platform on AWS us-east-1: "
        "2x m5.4xlarge ETL servers, 1x db.r6g.2xlarge PostgreSQL RDS Multi-AZ, "
        "5TB gp3 EBS for staging data. What is the total monthly cost?"
    ),
    "CX13": (
        "I need to run the same stack in three AWS regions for global coverage: "
        "us-east-1, eu-west-1, and ap-northeast-1. "
        "Stack per region: 2x m5.xlarge + 1x db.t4g.large MySQL RDS Single-AZ + 100GB gp3 EBS. "
        "What is the total combined monthly cost? Break down by region and show which is most expensive."
    ),
    "CX14": (
        "Disaster recovery budget for AWS: primary stack on us-east-1 "
        "(3x m5.2xlarge + 1x db.r6g.xlarge MySQL Multi-AZ + 500GB gp3 EBS), "
        "hot standby on us-west-2 (identical stack). "
        "What is the total monthly DR bill and how do the two regions compare in price?"
    ),
    # -----------------------------------------------------------------------
    # Azure Complex BoM/TCO (AZX)
    # -----------------------------------------------------------------------
    "AZX1": (
        "Give me the monthly TCO for a 3-tier application on Azure eastus: "
        "3x Standard_D4s_v3 web servers, 2x Standard_D8s_v3 app servers, "
        "1x Standard_E8s_v3 database server, 500GB premium-ssd storage. "
        "What should I budget?"
    ),
    "AZX2": (
        "I'm running a production workload on Azure westeurope: "
        "4x Standard_B4ms web servers, 1x Standard_D8s_v4 app server, 200GB premium-ssd. "
        "Estimate total monthly on-demand cost."
    ),
    "AZX3": (
        "Compare Azure eastus vs westeurope for this stack: "
        "2x Standard_D4s_v3 + 1x Standard_E4s_v3 + 200GB premium-ssd. "
        "What is the monthly cost in each region and which is cheaper?"
    ),
    # -----------------------------------------------------------------------
    # Multi-region full-stack comparisons (MRS)
    # -----------------------------------------------------------------------
    "MRS1": (
        "I run a 3-tier stack on AWS: 2x m5.xlarge web servers, 1x db.r6g.large MySQL RDS Single-AZ, "
        "200GB gp3 EBS. Compare the total monthly cost across us-east-1, eu-west-1, and ap-southeast-1. "
        "Which region is cheapest overall and by how much?"
    ),
    "MRS2": (
        "Compare total monthly cost for a GCP stack (2x n2-standard-4 + 500GB pd-ssd) "
        "across us-central1, europe-west1, and asia-east1. "
        "Which region is cheapest and what is the % premium for the most expensive?"
    ),
    "MRS3": (
        "Cross-cloud cross-region BoM: price this stack in four locations simultaneously — "
        "AWS us-east-1, AWS eu-west-1, GCP us-central1, GCP europe-west1. "
        "Stack: 2x 4-vCPU/16GB web servers + 500GB SSD block storage. "
        "Rank all four options cheapest to most expensive with monthly totals."
    ),
    # -----------------------------------------------------------------------
    # Cross-cloud BoM comparisons (CCR)
    # -----------------------------------------------------------------------
    "CCR1": (
        "Price this identical 3-tier stack across all three clouds in their US East regions "
        "(AWS us-east-1, GCP us-central1, Azure eastus): "
        "4x general-purpose 4 vCPU/16GB web servers, 1x 8 vCPU/32GB database server, "
        "500GB SSD block storage. Use real instance types. Which cloud is cheapest overall?"
    ),
    "CCR2": (
        "Compare total monthly cost for a 3-tier stack across all three clouds in US regions: "
        "3x 4-vCPU/16GB web servers + 1x 8-vCPU/32GB database + 500GB SSD. "
        "Show both on-demand and 1-year committed pricing. "
        "Which cloud offers the best committed pricing and what is the saving vs on-demand?"
    ),
    "CCR3": (
        "I'm choosing a cloud provider for a new product. Compare monthly cost across AWS, GCP, and Azure "
        "for this stack: 6x 4-vCPU/16GB app servers, 2x 8-vCPU/64GB memory-optimised DB servers, "
        "1TB SSD block storage, in US East regions. Show itemised costs per provider."
    ),
    # -----------------------------------------------------------------------
    # Azure Simple (AZ)
    # -----------------------------------------------------------------------
    "AZ1": (
        "What does a Standard_D4s_v3 cost on-demand in eastus? Give me the hourly and monthly figure."
    ),
    "AZ2": (
        "Compare Standard_D4s_v3 on-demand vs 1-year reserved pricing in eastus. "
        "What is the monthly saving and percentage discount?"
    ),
    "AZ3": (
        "I need 500GB of Azure premium-ssd block storage in westeurope. What's the monthly cost?"
    ),
    "AZ4": (
        "Which Azure region is cheapest for a Standard_E8s_v3 (memory-optimised, 8 vCPU / 64GB)?"
    ),
    "AZ5": (
        "What's the price difference between running a Standard_D4s_v3 on Linux vs Windows in eastus? "
        "Show both hourly rates and the Windows premium."
    ),
    # -----------------------------------------------------------------------
    # Advanced AWS (AA)
    # -----------------------------------------------------------------------
    "AA1": (
        "What does AWS Lambda cost per 1 million requests in us-east-1? "
        "Also show the GB-second duration price. How much would 10M requests at 512MB / 200ms cost?"
    ),
    "AA2": ("I need to store 50TB in S3 Standard in us-east-1. What is the monthly storage cost?"),
    "AA3": (
        "Compare db.r6g.large MySQL RDS Single-AZ vs Multi-AZ monthly cost in us-east-1. "
        "What is the extra monthly cost of high availability?"
    ),
    "AA4": (
        "I want to commit to a 3-year All Upfront reserved m5.2xlarge in us-east-1. "
        "What is the effective hourly rate and how many months until break-even vs on-demand?"
    ),
    "AA5": (
        "List all GPU instances available in us-east-1 on AWS and show their on-demand prices. "
        "Which is the cheapest GPU instance per hour?"
    ),
    # -----------------------------------------------------------------------
    # Multi-Cloud 3-way comparisons (MC)
    # -----------------------------------------------------------------------
    "MC1": (
        "Compare on-demand pricing for a 4 vCPU / 16GB general-purpose instance across all three clouds "
        "in US regions: AWS m5.xlarge (us-east-1), GCP n2-standard-4 (us-central1), "
        "Azure Standard_D4s_v3 (eastus). Which is cheapest per hour and per month?"
    ),
    "MC2": (
        "Compare 1-year committed pricing across AWS, GCP, and Azure for a 4 vCPU / 16GB instance "
        "in US regions (reserved_1yr for AWS/Azure, cud_1yr for GCP). "
        "Which cloud offers the biggest discount off on-demand?"
    ),
    "MC3": (
        "Compare 500GB SSD block storage monthly cost across all three clouds: "
        "AWS gp3 (us-east-1), GCP pd-ssd (us-central1), Azure premium-ssd (eastus). "
        "Which is cheapest and by how much?"
    ),
    "MC4": (
        "I'm building a 2-tier app: 4x 4-vCPU web servers plus 500GB SSD storage. "
        "Compare total monthly cost on AWS (us-east-1), GCP (us-central1), and Azure (eastus). "
        "Use real instance types for each cloud and pick the closest 4 vCPU / 16GB match."
    ),
    "MC5": (
        "What's the cheapest cloud for a memory-optimised 8 vCPU / 64GB instance in a US region? "
        "Check AWS (r6i.2xlarge, us-east-1), GCP (n2-highmem-8, us-central1), "
        "and Azure (Standard_E8s_v3, eastus) on-demand pricing."
    ),
    # -----------------------------------------------------------------------
    # GCP GKE — Kubernetes Engine (GK) [v0.7.2]
    # -----------------------------------------------------------------------
    "GK1": (
        "What does a GKE Standard cluster management fee cost per month in us-central1? "
        "I have 3x n2-standard-4 worker nodes. What should I budget for the full cluster?"
    ),
    "GK2": (
        "I want to run GKE Autopilot in us-central1 with pods requesting 4 vCPU and 16GB RAM. "
        "What is the hourly and monthly cost for those pod resources?"
    ),
    "GK3": (
        "Compare GKE Standard vs Autopilot for running a workload of 4 vCPU / 16GB in us-central1. "
        "Which billing model is cheaper if the workload runs 24/7?"
    ),
    "GK4": (
        "I'm running a GKE Standard cluster in us-central1 with 10x n2-standard-4 nodes. "
        "What is the cluster management fee per month, and what are the node compute costs separately?"
    ),
    "GK5": (
        "What does GKE Autopilot cost for a small microservice in us-central1 "
        "requesting 0.5 vCPU and 512MB RAM running always-on?"
    ),
    # -----------------------------------------------------------------------
    # GCP Memorystore for Redis (GM) [v0.7.2]
    # -----------------------------------------------------------------------
    "GM1": ("What does a 10GB Memorystore for Redis Basic instance cost per month in us-central1?"),
    "GM2": (
        "Compare Memorystore for Redis Basic vs Standard (HA) for a 10GB cache in us-central1. "
        "What is the monthly cost difference and what does Standard add?"
    ),
    "GM3": (
        "I need a 50GB Redis cache with high availability in us-central1. "
        "What does Memorystore Standard tier cost per month?"
    ),
    "GM4": (
        "What's the monthly cost of a 100GB Memorystore for Redis Standard instance in europe-west1?"
    ),
    "GM5": (
        "I need a session cache of about 5GB in us-central1. "
        "What does Memorystore Basic tier cost and how does it compare to Standard?"
    ),
    # -----------------------------------------------------------------------
    # GCP BigQuery (GB) [v0.7.3]
    # -----------------------------------------------------------------------
    "GB1": (
        "How much does BigQuery charge to query 10TB of data in the us multi-region? "
        "What is the per-TiB rate and the total cost?"
    ),
    "GB2": (
        "What is the monthly cost of storing 500GB of active data in BigQuery in us-central1? "
        "How does that compare to long-term storage pricing?"
    ),
    "GB3": (
        "I have 1TB of BigQuery data that hasn't been modified in 90 days — it qualifies for long-term storage. "
        "What is the monthly saving vs active storage pricing?"
    ),
    "GB4": (
        "My BigQuery workload: 5TB queries/month, 200GB active storage, 100GB long-term storage, "
        "10GB streaming inserts. What is the total estimated monthly cost in us?"
    ),
    "GB5": (
        "What does BigQuery streaming insert pricing look like? "
        "If I stream 50GB of data per month, what is the cost?"
    ),
    # -----------------------------------------------------------------------
    # GCP Vertex AI + Gemini (GV) [v0.7.4]
    # -----------------------------------------------------------------------
    "GV1": (
        "How much does Vertex AI custom training on an n1-standard-8 cost "
        "for a 100-hour training job in us-central1?"
    ),
    "GV2": (
        "I want to run Vertex AI training on an a2-highgpu-1g GPU instance "
        "for 50 hours in us-central1. What is the estimated cost?"
    ),
    "GV3": (
        "What are the input and output token rates for Gemini 1.5 Flash in us-central1? "
        "Show both rates and the cost per million tokens."
    ),
    "GV4": (
        "I plan to use Gemini 1.5 Flash for a chatbot processing 50 million input tokens "
        "and 10 million output tokens per month. What is the monthly API cost?"
    ),
    "GV5": (
        "Compare Vertex AI training cost for n1-standard-4 vs n1-standard-8 "
        "for a 200-hour monthly training job in us-central1. Which is cheaper per hour?"
    ),
    "GV6": (
        "How much does it cost to run a GCP n1-standard-8 in us-central1 for an entire month "
        "of continuous 24/7 operation, with GCP's Sustained Use Discount applied? "
        "Show me the per-tier breakdown and compare the SUD effective rate to the on-demand "
        "and 1-year CUD rates."
    ),
    "GV7": (
        "What is the cost of running a GCP n2-standard-8 VM in us-central1 for a full month "
        "under a flexible committed use discount (no long-term commitment required)? Compare "
        "this to on-demand and 1-year CUD pricing and tell me which is cheapest."
    ),
    "GV8": (
        "How much does it cost per hour to run an a2-highgpu-1g instance in us-central1 on GCP? "
        "Make sure to include the A100 GPU cost in addition to the CPU and RAM cost."
    ),
    # -----------------------------------------------------------------------
    # GCP Networking — LB, CDN, NAT (GN) [v0.7.6]
    # -----------------------------------------------------------------------
    "GN1": (
        "What does an External HTTPS Cloud Load Balancer cost in us-central1? "
        "I have 2 forwarding rules and process 1TB of traffic per month."
    ),
    "GN2": (
        "How much does Cloud CDN cost for 5TB of monthly cache egress to North America from us-central1?"
    ),
    "GN3": (
        "I have 2 Cloud NAT gateways in us-central1 processing 500GB of data per month. "
        "What is the monthly cost including gateway uptime and data processing charges?"
    ),
    "GN4": (
        "What is the per-GB rate for Cloud CDN cache fill vs cache egress? "
        "If I serve 10TB from CDN with a 60% cache hit ratio, estimate the monthly cost."
    ),
    "GN5": (
        "Estimate the full monthly networking cost for a GCP web app in us-central1: "
        "1 HTTPS load balancer (3 rules, 2TB traffic), Cloud CDN (3TB egress), "
        "1 NAT gateway (200GB processed)."
    ),
    # -----------------------------------------------------------------------
    # GCP Cloud Armor + Cloud Monitoring (GC) [v0.7.7]
    # -----------------------------------------------------------------------
    "GC1": (
        "What does Cloud Armor Standard cost for 2 enforced security policies "
        "receiving 100 million requests per month?"
    ),
    "GC2": (
        "How much does Cloud Monitoring cost if I ingest 500 MiB of custom metrics per month? "
        "Is any of that free?"
    ),
    "GC3": (
        "I'm ingesting 200,000 MiB of GCP Cloud Monitoring metrics per month. "
        "Walk me through the tiered pricing and give me the total monthly cost."
    ),
    "GC4": (
        "At what monthly request volume does GCP Cloud Armor Standard become more expensive "
        "than just using a basic WAF? Show the per-policy and per-request costs."
    ),
    "GC5": (
        "What is the combined monthly cost of Cloud Armor (3 policies, 500M requests) "
        "plus Cloud Monitoring (1000 MiB ingestion) for a production GCP deployment?"
    ),
    # -----------------------------------------------------------------------
    # GCP Complex stacks combining new services (GCX) [v0.7.2–v0.7.7]
    # -----------------------------------------------------------------------
    "GCX1": (
        "Estimate the monthly cost of a GCP data warehouse in us-central1: "
        "4x n2-standard-4 for ETL compute, 1TB active BigQuery storage, "
        "20TB BigQuery queries per month, 50GB streaming inserts."
    ),
    "GCX2": (
        "GCP ML platform monthly cost in us-central1: "
        "Vertex AI training on a2-highgpu-1g (80 hrs/month), "
        "2x n1-standard-4 for prediction serving (always-on), "
        "500GB active BigQuery storage, 5TB BigQuery queries."
    ),
    "GCX3": (
        "Monthly cost for a GKE Autopilot workload in us-central1 "
        "with 8 vCPU / 32GB pod requests, a 20GB Memorystore Standard cache, "
        "and Cloud Armor (1 policy, 50M requests/month)."
    ),
    "GCX4": (
        "Full GCP production stack in us-central1: "
        "GKE Standard cluster (3x n2-standard-4 nodes), "
        "Cloud SQL db-n1-standard-4 MySQL (single-zone), "
        "10GB Memorystore Basic cache, "
        "1 HTTPS load balancer (2TB/month traffic). "
        "What is the total monthly cost?"
    ),
    "GCX5": (
        "Complete monthly cost for a GCP web application in us-central1: "
        "2x n2-standard-4 app servers, 500GB pd-ssd storage, "
        "1 HTTPS load balancer (1 rule, 1TB traffic), "
        "2TB Cloud CDN egress, Cloud Armor (1 policy, 100M requests), "
        "300 MiB Cloud Monitoring ingestion. "
        "Break down each component and give a total."
    ),
    # -----------------------------------------------------------------------
    # GCP Cloud Storage — GCS (GGCS) [v0.7.x]
    # -----------------------------------------------------------------------
    "GGCS1": (
        "What does storing 1TB of data in GCS Standard storage cost per month in us-central1?"
    ),
    "GGCS2": (
        "Compare GCS storage class pricing for 500GB in us-central1: "
        "Standard vs Nearline vs Coldline vs Archive. Which is cheapest and what are the trade-offs?"
    ),
    "GGCS3": ("I store 200GB in GCS Nearline in europe-west1. What is the monthly storage cost?"),
    "GGCS4": (
        "My app uses GCS in us-central1: 100GB Standard, 500GB Nearline, 1TB Coldline. "
        "What is the total monthly storage cost broken down by class?"
    ),
    "GGCS5": (
        "What is the cheapest GCP GCS storage class for archival data accessed less than once a year? "
        "Show the per-GB rate and monthly cost for 10TB in us-central1."
    ),
    # -----------------------------------------------------------------------
    # GCP Cloud SQL (GSQL) [v0.7.x]
    # -----------------------------------------------------------------------
    "GSQL1": (
        "What does a Cloud SQL db-n1-standard-4 MySQL instance cost per month in us-central1?"
    ),
    "GSQL2": (
        "Compare Cloud SQL db-n1-standard-2 vs db-n1-standard-4 for MySQL in us-central1. "
        "What is the monthly cost difference?"
    ),
    "GSQL3": (
        "How much does a Cloud SQL PostgreSQL db-n1-standard-8 instance cost per month in europe-west1?"
    ),
    "GSQL4": (
        "What is the monthly cost difference between a single-zone and high-availability (HA) "
        "Cloud SQL db-n1-standard-4 MySQL in us-central1?"
    ),
    "GSQL5": (
        "I need a Cloud SQL MySQL instance in us-central1 with at least 4 vCPUs. "
        "What instance type should I use, and what is the monthly cost?"
    ),
    # -----------------------------------------------------------------------
    # Azure SQL / MySQL / PostgreSQL (AZSQL) [v0.8.8]
    # -----------------------------------------------------------------------
    "AZSQL1": (
        "What does an Azure SQL Database General Purpose 4 vCores instance cost per month in eastus?"
    ),
    "AZSQL2": (
        "Compare Azure SQL Database single-az vs HA (zone-redundant) pricing for "
        "General Purpose 4 vCores in eastus. What is the monthly cost difference?"
    ),
    "AZSQL3": (
        "What does an Azure Database for PostgreSQL General Purpose 8 vCores cost in westeurope? "
        "Give me the hourly and monthly figure."
    ),
    "AZSQL4": (
        "Compare on-demand vs 1-year reserved pricing for an Azure SQL Database "
        "Business Critical 8 vCores in eastus. What is the monthly saving?"
    ),
    "AZSQL5": (
        "I'm migrating from AWS RDS db.r6g.large MySQL (us-east-1) to Azure Database for MySQL "
        "in eastus. What is the closest Azure SKU and how do the monthly costs compare?"
    ),
    # -----------------------------------------------------------------------
    # Azure Cosmos DB (AZCOS) [v0.8.8]
    # -----------------------------------------------------------------------
    "AZCOS1": (
        "What does Azure Cosmos DB provisioned throughput cost per 100 RU/s in eastus? "
        "I need 10,000 RU/s — what is my monthly bill?"
    ),
    "AZCOS2": (
        "Compare Azure Cosmos DB provisioned vs serverless pricing in eastus. "
        "For a workload with 50M requests/month, which is cheaper?"
    ),
    "AZCOS3": (
        "What is the cost of Azure Cosmos DB with multi-region writes enabled in eastus? "
        "How does this compare to single-region pricing?"
    ),
    "AZCOS4": (
        "I'm building a serverless app on Azure. Compare the cost of Azure Cosmos DB serverless "
        "vs Azure SQL Database General Purpose 2 vCores in eastus for a low-traffic workload."
    ),
    "AZCOS5": ("What would a Cosmos DB autoscale setup cost in eastus? Explain the pricing model."),
    # -----------------------------------------------------------------------
    # Azure Kubernetes Service (AZAKS) [v0.8.8]
    # -----------------------------------------------------------------------
    "AZAKS1": (
        "What does an AKS cluster cost per month in eastus? Include the control plane fee "
        "and estimate for 3x Standard_D4s_v3 worker nodes."
    ),
    "AZAKS2": (
        "Compare AKS free tier vs Standard tier (Uptime SLA) in eastus. "
        "What is the monthly cost difference for the control plane?"
    ),
    "AZAKS3": (
        "I need a production AKS cluster in westeurope with 5x Standard_D8s_v3 nodes. "
        "What is the total monthly estimate including the cluster management fee?"
    ),
    # -----------------------------------------------------------------------
    # Azure Functions (AZFN) [v0.8.8]
    # -----------------------------------------------------------------------
    "AZFN1": (
        "What does Azure Functions cost on the Consumption plan in eastus? "
        "Give me the per-GB-second and per-execution rates."
    ),
    "AZFN2": (
        "My Azure Function runs 10 million times a month, each execution using 512MB for 500ms. "
        "What is the monthly cost in eastus after the free tier?"
    ),
    "AZFN3": (
        "Compare AWS Lambda vs Azure Functions Consumption plan pricing for a workload with "
        "5M executions/month, 1GB memory, 1 second average duration. Which is cheaper?"
    ),
    "AZFN4": (
        "What is the Azure Functions free tier allowance, and at what scale does the "
        "Consumption plan become cost-significant? Give numbers for eastus."
    ),
    "AZFN5": (
        "I'm planning to migrate my AWS Lambda workload (us-east-1) to Azure Functions (eastus). "
        "Lambda runs 20M invocations/month at 256MB / 200ms average. "
        "What will I pay on Azure Functions vs AWS Lambda?"
    ),
    # -----------------------------------------------------------------------
    # Azure OpenAI (AZAI) [v0.8.8]
    # -----------------------------------------------------------------------
    "AZAI1": ("What does Azure OpenAI GPT-4o cost per 1K input and output tokens in eastus?"),
    "AZAI2": (
        "Compare Azure OpenAI GPT-4o vs GPT-4o-mini pricing in eastus. "
        "For a workload sending 1M input tokens and receiving 500K output tokens per month, "
        "what is the cost difference?"
    ),
    "AZAI3": (
        "I'm running a RAG pipeline on Azure OpenAI in eastus using text-embedding-3-small "
        "to embed 10M tokens per month. What is the monthly embedding cost?"
    ),
    "AZAI4": (
        "Compare Azure OpenAI o1 vs GPT-4o pricing in eastus. "
        "Which is more cost-effective for a reasoning-heavy workload?"
    ),
    "AZAI5": (
        "Build a monthly cost estimate for an Azure OpenAI-powered chatbot in eastus: "
        "GPT-4o-mini, 5M input tokens and 2M output tokens per month. "
        "What is the total monthly AI cost?"
    ),
    # -----------------------------------------------------------------------
    # Inter-region egress / data transfer (EGR) [v0.8.3]
    # -----------------------------------------------------------------------
    "EGR1": ("How much does AWS charge to transfer 1TB of data from us-east-1 to eu-west-1?"),
    "EGR2": (
        "Compare AWS inter-region data transfer costs: us-east-1 to eu-west-1 vs "
        "us-east-1 to ap-southeast-1 for 1TB. Which is more expensive and by how much?"
    ),
    "EGR3": (
        "I'm running a multi-region active-active setup on AWS: 5TB of data transferred "
        "between us-east-1 and eu-west-1 per month. What is the monthly egress bill?"
    ),
    "EGR4": (
        "What does GCP charge for inter-region egress from us-central1 to europe-west1 for 1TB?"
    ),
    "EGR5": (
        "Compare AWS vs GCP inter-region data transfer costs for moving 1TB from US East "
        "to EU regions. Which cloud is cheaper for cross-region traffic?"
    ),
    # --- GCP Storage / Database Contract Pricing (v0.8.11) ---
    "GCPSTO1": (
        "What is the on-demand price for GCS Standard storage in us-central1 per GB-month?"
    ),
    "GCPSTO2": (
        "Compare GCS Nearline vs Coldline vs Archive storage prices in us-central1. "
        "Which is cheapest for long-term archival?"
    ),
    "GCPSTO3": ("What does GCP pd-ssd Persistent Disk cost per GB-month in us-central1?"),
    "GCPDB1": (
        "What is the hourly cost for a Cloud SQL db-n1-standard-4 MySQL instance "
        "in us-central1 (zonal, no HA)?"
    ),
    "GCPDB2": (
        "Compare Cloud SQL db-n1-standard-4 MySQL: zonal vs regional (HA) pricing "
        "in us-central1. What is the monthly cost difference?"
    ),
    "GCPDB3": (
        "What does a Memorystore Redis standard-tier 8GB instance cost per hour in us-central1?"
    ),
    # --- GCP Network Pricing (v0.8.13) ---
    "GCPNET1": (
        "What does a GCP External HTTP(S) Load Balancer cost per month for 3 forwarding "
        "rules processing 500GB of data in us-central1?"
    ),
    "GCPNET2": (
        "What are the Cloud CDN egress and cache fill rates in us-central1? "
        "Estimate the monthly cost for 10TB egress and 2TB cache fill."
    ),
    "GCPNET3": (
        "What does a Cloud NAT gateway cost in us-central1 per month? "
        "Include the data processing charge for 1TB."
    ),
    # --- Azure Egress Pricing (v0.8.12) ---
    "AZEGR1": (
        "What does Azure charge for outbound internet data transfer from eastus? "
        "Give me the per-GB rate and the monthly cost for 1TB."
    ),
    "AZEGR2": (
        "I need to transfer 500GB of data from Azure East US to West Europe each month. "
        "What will that cost?"
    ),
    "AZEGR3": (
        "Compare AWS, GCP, and Azure internet egress costs for 1TB from US East regions. "
        "Which cloud charges the least for outbound data?"
    ),
    # v0.8.14 — GCP inter-region egress
    "GCPEGR1": (
        "What is the GCP internet egress cost for transferring 500 GB/month from us-central1?"
    ),
    "GCPEGR2": (
        "How much does it cost to transfer 1 TB/month between a GCP region in the US (us-central1) "
        "and one in Europe (europe-west1)? Compare with internet egress from the same source."
    ),
    "GCPEGR3": (
        "Compare internet egress costs for 1 TB/month from AWS (us-east-1), "
        "GCP (us-central1), and Azure (eastus). Which is cheapest?"
    ),
    # -----------------------------------------------------------------------
    # Azure Monitor / Log Analytics (AZMON) [v0.8.x]
    # -----------------------------------------------------------------------
    "AZMON1": (
        "What does Azure Monitor charge for log ingestion in eastus? "
        "I plan to ingest 100 GB of Analytics Logs per month. What is the monthly cost after the free tier?"
    ),
    "AZMON2": (
        "Compare Azure Monitor Analytics Logs vs Basic Logs pricing per GB in eastus. "
        "Which log type should I use for high-volume, query-intensive workloads?"
    ),
    "AZMON3": (
        "I have 50 Azure Monitor metric alert rules in eastus. "
        "What is the monthly cost for those alert rules after the free tier?"
    ),
    # -----------------------------------------------------------------------
    # Azure CDN / Front Door (AZCDN / AZFD) [v0.8.x]
    # -----------------------------------------------------------------------
    "AZCDN1": (
        "What does Azure CDN Standard cost to serve 5TB of data per month from eastus? "
        "Give me the per-GB rate and total monthly estimate."
    ),
    "AZFD1": (
        "What does Azure Front Door Standard cost per month in eastus for a workload "
        "serving 10TB of data and 500 million requests? Use get_price with service='azure_front_door', "
        "domain='network', data_gb=10000, monthly_requests_millions=500. "
        "The response includes a pre-computed estimated monthly cost row with a breakdown — report that total."
    ),
    "AZFD2": (
        "Compare Azure CDN vs Azure Front Door pricing for serving 1TB/month of content "
        "to users primarily in North America and Europe. Which is cheaper and what are the trade-offs?"
    ),
    # -----------------------------------------------------------------------
    # New Azure service spot checks (NSV) — added with v0.8.x critical fixes
    # -----------------------------------------------------------------------
    "NSV1": (
        "What does an Azure SQL Database General Purpose 4 vCores instance cost per hour "
        "and per month in eastus? Include single-az pricing."
    ),
    "NSV2": (
        "My Azure Function on the Consumption plan runs 10 million times per month, "
        "each using 512MB memory for 500ms. What is the monthly cost in eastus after the free tier?"
    ),
    "NSV3": (
        "What is the AKS control plane fee for a Standard tier cluster in eastus? "
        "How much would 3x Standard_D4s_v3 worker nodes add to the monthly cost?"
    ),
    "NSV4": (
        "What does Azure OpenAI GPT-4o cost for 1 million input tokens and 500K output tokens "
        "per month in eastus? Show input and output rates separately."
    ),
    "NSV5": (
        "How much does Azure Monitor Log Analytics ingestion cost for 100 GB per month in eastus? "
        "What is the monthly cost after the free tier?"
    ),
    "NSV6": (
        "What does Azure CDN Standard cost to serve 1 TB of data per month from eastus (Zone 1)? "
        "Give the per-GB rate and the total monthly estimate."
    ),
    # --- Egress Tiering (NET_EGR) [network/egress domain] ---
    "NET_EGR1": (
        "What is the total monthly cost for sending 5 TB of outbound internet traffic from AWS us-east-1? "
        "Show the per-tier breakdown (first 100 GB free, then tiered rates) and give me the blended effective rate per GB. "
        "Call get_price with spec={\"provider\":\"aws\",\"domain\":\"network\",\"service\":\"egress\","
        "\"source_region\":\"us-east-1\",\"destination_type\":\"internet\",\"data_gb_per_month\":5000}."
    ),
    "NET_EGR2": (
        "How much does it cost to transfer 1 TB of data each month from AWS us-east-1 to "
        "eu-west-1? Use domain=network, service=egress, destination_type=cross_region."
    ),
    "NET_EGR3": (
        "What is the GCP internet egress cost for 2 TB/month from us-central1? "
        "Show the tiered breakdown (rates change at 1 TB and 10 TB) and the blended rate. "
        "Use domain=network, service=egress, destination_type=internet, network_tier=premium, data_gb_per_month=2048."
    ),
    "NET_EGR4": (
        "What does Azure charge for 1 TB of outbound internet traffic from eastus per month? "
        "Show the tier breakdown (first 5 GB free, then tiered rates) and total monthly cost. "
        "Use domain=network, service=egress."
    ),
    # --- New egress prompts (NET_EGR5-NET_EGR8) ---
    "NET_EGR5": (
        "What would it cost to transfer 5 TB per month out of AWS us-east-1 to the internet? "
        "Use domain=network, service=egress and show the tiered cost breakdown."
    ),
    "NET_EGR6": (
        "How much does it cost to transfer 1 TB/month from us-east-1 to eu-west-1 on AWS? "
        "Use domain=network, service=egress, destination_type=cross_region."
    ),
    "NET_EGR7": (
        "Estimate GCP egress cost for 2 TB/month from us-central1 to users in Europe. "
        "Use domain=network, service=egress, destination_type=internet, network_tier=premium."
    ),
    "NET_EGR8": (
        "What is the Azure bandwidth cost for 1 TB/month outbound from East US? "
        "Use domain=network, service=egress and show the tier breakdown."
    ),
    # ── New coverage: trust metadata, cross-cloud egress, Azure expansion, resilience ──
    "TRUST1": (
        "How fresh is the AWS pricing data for EC2 compute in us-east-1? "
        "Call get_price with provider=aws, domain=compute, resource_type=m5.xlarge, region=us-east-1, "
        "term=on_demand, os=Linux. Report the as_of date and source_url from the tool response. "
        "AWS responses include as_of (the timestamp when the pricing data was last fetched) and source_url, "
        "but do not include a cache_age_seconds field — as_of is the freshness indicator."
    ),
    "TRUST2": (
        "Get GCP Compute Engine pricing for n2-standard-4 in us-central1. "
        "Report the as_of date, source_url, and cache_age_seconds from the tool response."
    ),
    "EGR_X1": (
        "I'm moving 50 TB/month of data from AWS us-east-1 to the internet. "
        "What is the tiered egress cost breakdown? "
        "Call get_price with spec: {\"provider\": \"aws\", \"domain\": \"network\", \"service\": \"egress\", "
        "\"source_region\": \"us-east-1\", \"destination_type\": \"internet\", \"data_gb_per_month\": 51200}. "
        "Report the tier breakdown (GB per tier, rate per GB, cost per tier) and the total monthly cost."
    ),
    "EGR_X2": (
        "Compare internet egress costs for 10 TB/month from: AWS us-east-1, "
        "GCP us-central1, and Azure eastus. Which is cheapest? "
        "Use domain=network, service=egress for each provider."
    ),
    "EGR_X3": (
        "What does inter-region data transfer cost within AWS from us-east-1 to us-west-2 "
        "for 5 TB/month? Use domain=inter_region_egress with source_region=us-east-1 "
        "and dest_region=us-west-2."
    ),
    "AZAKS4": (
        "What does an AKS cluster with a spot node pool cost in eastus? "
        "I want 3x Standard_D4s_v3 spot VMs as worker nodes. "
        "Compare spot vs on-demand pricing for the same VM size."
    ),
    "AZFN6": (
        "What does Azure Functions Premium plan (EP1) cost per month in eastus? "
        "I need always-on execution with 1 pre-warmed instance and expect 5 million "
        "additional executions. How does this compare to the Consumption plan cost?"
    ),
    "AZAI6": (
        "What is the Azure OpenAI o3-mini pricing in eastus for a reasoning workload "
        "sending 2 million input tokens and receiving 1 million output tokens per month?"
    ),
    "FCR1": (
        "Which AWS region is cheapest for a c6g.4xlarge Linux on-demand instance? "
        "Check only these regions: us-east-1, us-west-2, eu-west-1, ap-southeast-1."
    ),
    "FCR2": (
        "Find the cheapest GCP region for an n2-standard-8 VM. "
        "Limit to: us-central1, us-east1, europe-west1, asia-east1."
    ),
    "BOM_RES1": (
        "What is the 1-year No Upfront reserved price for 3x m5.xlarge Linux in us-east-1? "
        "Compare it to on-demand and tell me the annual savings."
    ),
    "BOM_RES2": (
        "Estimate monthly cost for a 3-tier AWS stack with 1-year No Upfront reservations: "
        "2x c5.2xlarge Linux web (us-east-1), 1x r6i.xlarge Linux DB (us-east-1), 500 GB gp3 EBS."
    ),
    "REC1": ("Price a t3.medium Linux instance."),
    # -----------------------------------------------------------------------
    # AWS Savings Plans + EDP (AV)
    # -----------------------------------------------------------------------
    "AV1": (
        "Compare the cost of running an m5.4xlarge (16 vCPU, 64 GB RAM) in us-east-1 on AWS: "
        "on-demand vs 1-year Compute Savings Plan vs 3-year Compute Savings Plan. "
        "Show hourly and monthly costs and the discount percentage for each."
    ),
    "AV2": (
        "I have a 1-year AWS Compute Savings Plan. Will it cover costs for both m5 instances "
        "in us-east-1 AND c5 instances in us-west-2 simultaneously? Explain how CSP flexibility "
        "works and show the rates for c5.2xlarge in both regions."
    ),
    "AV3": (
        "What is the Compute Savings Plan rate for a c7g.4xlarge (Graviton3, ARM) instance in "
        "us-east-1 for a 1-year term? How does it compare to on-demand?"
    ),
    "AV4": (
        "I need 64 vCPU and 256 GB RAM of compute capacity on AWS for 1 year. Compare using "
        "m5.16xlarge on-demand vs 1yr Compute SP vs 3yr Compute SP. "
        "What is the total annual cost for each?"
    ),
    "AV5": (
        "I always run m5 instances in us-east-1 and will continue to do so for the next year. "
        "Should I choose a Compute Savings Plan or an EC2 Instance Savings Plan? "
        "Show the rates for m5.4xlarge under each plan type."
    ),
    "AV6": (
        "Compare 1-year vs 3-year EC2 Instance Savings Plan (No Upfront) pricing for r5.4xlarge in us-east-1. "
        "Show the effective hourly rate and estimated monthly cost for each commitment term. "
        "Then calculate: (a) monthly savings of the 3-year plan versus the 1-year plan, and "
        "(b) total savings over 36 months assuming the 3-year rate versus renewing the 1-year plan three times."
    ),
    "AV7": (
        "What is the cost of running a Windows Server m6i.4xlarge in us-east-1 under a "
        "1-year EC2 Instance Savings Plan? Compare to Windows on-demand pricing."
    ),
    "AV8": (
        "Our company has a 20% AWS Enterprise Discount Program agreement. What is our effective "
        "hourly rate for m5.4xlarge in us-east-1 under a 1-year Compute Savings Plan with the "
        "EDP applied?"
    ),
    "AV9": (
        "We have 15% EDP on AWS. Compare our effective rate for 16 vCPU 64GB: AWS m5.4xlarge "
        "with 3yr EC2 Instance SP + EDP vs GCP n2-standard-16 with 3yr CUD. "
        "Which is cheaper per month?"
    ),
    "AV10": (
        "What is the absolute maximum discount achievable on EC2 m5.4xlarge in us-east-1? "
        "Walk through: on-demand baseline, then best Savings Plan rate, then with a 25% EDP "
        "on top. Show the final effective rate and total savings %."
    ),
    "AV11": (
        "Compare the cheapest monthly cost for 8 vCPU 32 GB compute workload: AWS c5.2xlarge "
        "with best Savings Plan vs GCP c2-standard-8 with best CUD. Use us-east-1 for AWS and "
        "us-central1 for GCP. Assume Linux, 3-year commitment."
    ),
    "AV12": (
        "We are choosing between AWS and GCP for a 3-year memory-optimized database workload "
        "requiring 16 vCPU and 128 GB RAM. Compare AWS r5.4xlarge (best SP rate) vs GCP "
        "n2-highmem-16 (best CUD). Include all available discounts."
    ),
    "AV13": (
        "Compare GPU instance pricing: AWS p3.2xlarge (1x V100 GPU, 8 vCPU, 61 GB RAM) vs "
        "GCP a2-highgpu-1g (1x A100 GPU, 12 vCPU, 85 GB RAM) in us-east-1/us-central1. "
        "Include any applicable Savings Plans or CUDs."
    ),
    "AV14": (
        "Total 3-year cost of ownership for running 100 general-purpose VMs each with "
        "4 vCPU 16 GB RAM on AWS vs GCP. Use AWS m5.xlarge with best discount vs GCP "
        "n2-standard-4 with best discount. Assume Linux, us-east-1 / us-central1."
    ),

    # --- Cluster / Workload ---
    "CL1": (
        "Price a baseline EKS cluster in us-east-1. Tag each line item as"
        " recurring (monthly) or one-time.\n\n"
        "Worker nodes:\n"
        "- 6× m6a.xlarge on-demand (general-purpose)\n"
        "- 2× r6i.xlarge on-demand (memory-optimized)\n"
        "- 2× m6a.xlarge spot (batch) — call get_spot_price_history and note"
        " the region, AZ, and timestamp of the data returned\n\n"
        "Storage: one 100 GB EBS gp3 root volume per node (10 volumes total) at"
        " default IOPS (3,000) and throughput (125 MB/s)\n\n"
        "Networking: 100 GB/month outbound internet egress as a baseline"
        " data-transfer charge\n\n"
        "EKS control plane: the public rate is $0.10/hr; use this value if it"
        " is not present in your pricing catalog, and note whether you sourced"
        " it from the catalog or used the hardcoded public rate\n\n"
        "CloudWatch: attempt to price a basic monitoring baseline for 10 nodes;"
        " if CloudWatch is not available in your pricing catalog, state this"
        " explicitly and exclude it from the total\n\n"
        "Show the control plane as a separate flat line item, distinct from all"
        " compute charges. Produce a per-node cost breakdown, then roll up to a"
        " single cluster monthly total."
    ),
    "CL2": (
        "Produce a monthly bill-of-materials for a portal/dashboard deployment"
        " in us-east-1. Tag each line item as recurring (monthly) or one-time,"
        " and keep primary storage costs separate from backup costs.\n\n"
        "Tiers to price:\n"
        "1. Web tier — 2 × m5.large EC2 Linux on-demand.\n"
        "2. Load balancer — 1 ALB, 730 hours/month. Look up the ALB price using"
        " get_price(spec={provider:\"aws\",domain:\"network\",service:\"lb\","
        "region:\"us-east-1\"}). The result returns two unit prices: an hourly"
        " base charge and an LCU-hour rate. Compute: (1) base_charge ="
        " hourly_rate × 730; (2) lcu_charge = lcu_hour_rate × 10 LCU × 730."
        " Report both as separate line items.\n"
        "3. Database tier — RDS db.m5.large MySQL 8.0 Multi-AZ, 200 GB gp3"
        " storage, 3 000 provisioned IOPS.\n"
        "4. Object storage — S3 Standard 500 GB storage (storage cost only;"
        " skip S3 per-request pricing if absent from catalog).\n"
        "5. Backup — RDS snapshot storage at 200 GB"
        " (storage_type=\"rds-snapshot\").\n\n"
        "Rule: if any line item is not found after a single get_price attempt,"
        " mark it as unavailable and move on. Use estimate_bom to roll up tiers"
        " 1 and 3 into a single call; price tiers 2, 4, and 5 individually."
    ),
    # --- Cluster / Workload ---
    "XK1": (
        "Price a 10-node Kubernetes cluster on AWS EKS and GCP GKE for a"
        " like-for-like comparison. Use us-east-1 (AWS) and us-east1 (GCP),"
        " on-demand pricing.\n\n"
        "AWS EKS config:\n"
        "- Control plane: 1 cluster (use describe_catalog to check if EKS"
        " control-plane pricing is in the catalog; if absent, apply the"
        " standard $0.10/hr rate manually)\n"
        "- Worker nodes: 10 x m5.xlarge\n"
        "- Storage: 10 x 100 GB gp3 EBS volumes (3000 IOPS, 125 MB/s"
        " throughput)\n\n"
        "GCP GKE config (Standard mode):\n"
        "- Control plane: 1 cluster (use describe_catalog to check; if absent,"
        " apply $0.10/hr manually)\n"
        "- Worker nodes: 10 x n2-standard-4\n"
        "- Storage: 10 x 100 GB pd-balanced persistent disks\n\n"
        "State your SKU equivalence assumptions explicitly for both node types"
        " and storage classes. Tag each line item as recurring (monthly) or"
        " one-time. Present a side-by-side table showing per-provider monthly"
        " totals, the absolute dollar delta, and the percentage difference."
    ),
    "XK2": (
        "Compare monthly infrastructure costs for a Kubernetes cluster across"
        " AWS EKS, GCP GKE Standard, and Azure AKS. Configuration: 3 worker"
        " nodes equivalent to 4 vCPU / 16 GB RAM each, on-demand pricing, in US"
        " East regions (us-east-1 / us-central1 / eastus). Price four"
        " components per provider: (1) control plane fee — use the container"
        " domain for each provider and note that AKS free tier has no"
        " control-plane charge while GKE Standard charges per cluster; (2)"
        " worker nodes using the closest native instance type — state your SKU"
        " equivalence assumptions; (3) 100 GB managed block storage per node"
        " (gp3 / pd-ssd / Premium SSD); (4) one standard load balancer (ALB /"
        " GCP LB / Azure Application Gateway). Present results as a"
        " three-column table with rows EKS | GKE | AKS. Tag each line item as"
        " recurring (monthly) or one-time. Where a provider has no direct"
        " equivalent (e.g. GKE Autopilot pod-level billing vs Standard node"
        " billing, AKS free vs Standard tier control plane), note the"
        " difference and explain the assumption used. Flag any component the"
        " tool could not price."
    ),
    "PDISK": (
        "Compare AWS and GCP block storage costs for three disk profiles. Use"
        " us-east-1 for AWS and us-central1 for GCP. State your SKU equivalence"
        " assumptions for each profile.\n\n"
        "Profile A (size-bound, 10 TB, low performance): price AWS gp3 at 10 TB"
        " / 3 000 IOPS / 125 MB/s and AWS sc1 at 10 TB; compare with GCP"
        " pd-standard at 10 TB and pd-balanced at 10 TB.\n\n"
        "Profile B (capacity + throughput, 2 TB, 500 MB/s): price AWS gp3 at 2"
        " TB / 500 MB/s throughput / 3 000 IOPS; compare with GCP pd-ssd at 2"
        " TB.\n\n"
        "Profile C (IOPS-bound, 500 GB, 64 000 IOPS): price AWS io2 at 500 GB /"
        " 64 000 IOPS; compare with GCP pd-extreme at 500 GB / 64 000 IOPS and"
        " GCP hyperdisk-extreme at 500 GB / 64 000 IOPS.\n\n"
        "For each volume, present the cost as separate line items for storage"
        " capacity ($), provisioned IOPS ($), and provisioned throughput ($),"
        " omitting axes that do not apply to that disk type. Tag every line"
        " item as recurring (monthly). End with a per-profile cost summary"
        " table identifying the lowest-cost option."
    ),
    "AURO1": (
        "Price a three-node Aurora PostgreSQL on-demand cluster in us-east-1: one writer"
        " (db.r6g.2xlarge) and two readers (db.r6g.large).\n\n"
        "Step 1 — instance pricing: call get_price twice using spec"
        " {\"provider\": \"aws\", \"domain\": \"database\", \"service\": \"rds\","
        " \"engine\": \"aurora-postgresql\", \"deployment\": \"single-az\","
        " \"region\": \"us-east-1\", \"term\": \"on_demand\", \"resource_type\": \"<type>\"}."
        " Each call returns two prices — one for Aurora Standard (storage='EBS Only',"
        " lower price) and one for Aurora I/O-Optimized (storage='Aurora IO Optimization Mode',"
        " higher price). Read the storage field to identify which is which. Do not make"
        " additional tool calls to disambiguate. Multiply the reader hourly price by 2 for"
        " two nodes, then by 730 for monthly totals.\n\n"
        "Step 2 — storage and I/O: Aurora storage pricing is not in this catalog;"
        " use these AWS published rates directly (do not call any tool for these):\n"
        "  - Aurora Standard storage: $0.10 per GB-month\n"
        "  - Aurora I/O-Optimized storage: $0.225 per GB-month\n"
        "  - Aurora Standard I/O requests: $0.20 per 1 million requests"
        " (I/O-Optimized has no per-request charge)\n"
        "  - Aurora backup storage beyond the 1-day free tier: $0.021 per GB-month\n\n"
        "Step 3 — compute totals: For 500 GB cluster storage, 10 million I/O requests/month,"
        " and 29 days of backup retention charged (30-day retention minus 1-day free),"
        " produce a monthly cost table for both Aurora Standard and Aurora I/O-Optimized modes."
        " Tag each line item as recurring (monthly). State which mode is less expensive at"
        " 10M requests/month and compute the monthly I/O volume at which the two modes break even."
    ),
    # --- AI Model Pricing ---
    "AIMOD1": (
        "Compare per-1M-token input and output pricing for Claude Opus 4.8"
        " across AWS Bedrock (us-east-1), GCP Vertex AI, and Azure. For each"
        " provider, check whether Claude Opus 4.8 or the nearest available"
        " version is listed in the catalog — note any providers where it is"
        " absent. For available providers and variants, retrieve: (1) standard"
        " on-demand input and output prices per 1M tokens; (2) batch inference"
        " pricing if offered; (3) prompt-caching rates (cache write and cache"
        " read) if listed. Present results as a table: Provider | Variant |"
        " Input $/1M tokens | Output $/1M tokens. State your SKU equivalence"
        " assumptions across providers — including any version name differences"
        " and whether batch and caching tiers represent comparable features on"
        " each platform. Explicitly note pricing gaps where a provider or"
        " variant returned no data."
    ),
    "AIMOD2": (
        "Compare the cost of Claude Sonnet 4.6 managed on Vertex AI versus"
        " Gemma 3 27B self-hosted on a GCP g2-standard-8 instance (L4 GPU) in"
        " us-central1.\n\n"
        "1. Call describe_catalog(provider='gcp', domain='ai',"
        " service='vertex_ai') to confirm which model ID corresponds to Claude"
        " Sonnet 4.6, then look up its input and output token prices per"
        " million tokens on Vertex AI.\n\n"
        "2. Look up the on-demand hourly price for a g2-standard-8 instance in"
        " us-central1.\n\n"
        "3. Explicitly state both cost models:\n"
        "   - Managed (Vertex AI): monthly_cost = (input_tokens_M ×"
        " input_price_per_M) + (output_tokens_M × output_price_per_M)\n"
        "   - Self-hosted (g2-standard-8): monthly_cost = hourly_rate × 730 ×"
        " utilization_fraction (no per-token variable component)\n\n"
        "4. Assume a 3:1 input-to-output token ratio. Derive the formula for"
        " total monthly tokens at which self-hosting at 100% utilization"
        " becomes cheaper than managed pricing, and solve it. Then state the"
        ' result as: "self-hosting breaks even at X million tokens/month (100%'
        ' utilization)."\n\n'
        "5. Tag each line item as recurring (monthly) or one-time. Note any"
        " assumptions about what the self-hosted cost excludes (e.g., storage,"
        " networking, model serving overhead)."
    ),
    # --- GPU / Accelerated Computing ---
    "GPU1": (
        "Look up on-demand hourly pricing for three H200 GPU compute nodes"
        " across clouds. Use these exact SKUs and regions:\n\n"
        "1. AWS p5e.48xlarge (8× H200) — us-east-1\n"
        "2. GCP a3-ultragpu-8g (8× H200) — us-central1; if this SKU is absent"
        " from the catalog, fall back to a3-highgpu-8g (8× H100) and explicitly"
        " note the GPU type substitution (H100, not H200)\n"
        "3. Azure ND H200 v5 (8× H200) — eastus\n\n"
        "For each SKU: state your SKU equivalence assumptions. Report the"
        " on-demand per-node hourly rate. Then normalize: divide the node rate"
        " by its actual GPU count to produce a per-GPU-hour figure. If any SKU"
        " carries a GPU count other than 8, flag it clearly. If any SKU is"
        " entirely absent from the catalog, note it and skip rather than"
        " estimate. Present results as a table with columns: Provider | SKU |"
        " GPU model | GPU count | $/node/hr | $/GPU/hr. Tag every line item as"
        " recurring (monthly)."
    ),
    "GPU2": (
        "Look up on-demand, 1-year CUD, and 3-year CUD prices for the following"
        " GCP GPU instance types in us-central1:\n\n"
        "- a3-ultragpu-8g (H200, 8 GPUs per node) — attempt this first; if not"
        " found in the catalog, note it as unavailable and continue\n"
        "- a3-highgpu-8g (H100, 8 GPUs per node)\n"
        "- a2-highgpu-1g (A100 40 GB, 1 GPU per node)\n"
        "- a2-ultragpu-1g (A100 80 GB, 1 GPU per node)\n"
        "- g2-standard-4 (L4, 1 GPU per node)\n\n"
        "For each instance type and each pricing term, report:\n"
        "1. Per-node-hour price (from the catalog)\n"
        "2. Per-GPU-hour price (node price divided by the GPU count above)\n\n"
        "Present results as a table with columns: Instance Type, GPU Model, GPU"
        " Count, Term, Node $/hr, GPU $/hr. If a CUD tier is unavailable for a"
        " given instance family, note it explicitly rather than omitting the"
        " row."
    ),
    # --- AWS Network Stack ---
    "NET_STACK1": (
        "Calculate the total monthly cost for a 50 TB/month traffic flow"
        " through an AWS network stack in us-east-1:\n\n"
        "- Application Load Balancer: 730 hours/month at 100 LCUs"
        " (domain=\"network\", service=\"lb\")\n"
        "- NAT Gateway: 730 hours/month + 50 TB (51 200 GB) of data processing"
        " (domain=\"network\", service=\"nat\")\n"
        "- Outbound data transfer: 50 TB (51 200 GB) out to the internet"
        " (domain=\"network\", service=\"data_transfer\" — this is the AWS"
        " internet egress charge, billed separately from NAT processing)\n\n"
        "Present a line-item breakdown with: ALB hourly charge, ALB LCU"
        " charge, NAT Gateway hourly charge, NAT Gateway data-processing"
        " charge (label this as NAT per-GB, distinct from data transfer),"
        " and outbound data-transfer charge with tiered per-GB rates shown."
        " Tag each line item as recurring (monthly). Sum to a grand total."
        " Note that NAT per-GB processing and internet egress are two separate"
        " charges that both apply to the same 50 TB of outbound traffic."
    ),
    "EGRESS_HV": (
        "Calculate monthly AWS egress costs for two separate line items, tagging each as recurring (monthly):\n\n"
        "1. Inter-region transfer — 6 PB (6,000,000 GB) from us-east-1 to us-west-2.\n"
        "   Call get_price(spec={\"provider\": \"aws\", \"domain\": \"inter_region_egress\","
        " \"source_region\": \"us-east-1\", \"dest_region\": \"us-west-2\"})."
        " Report the flat per-GB rate and total monthly cost (rate x 6,000,000 GB)."
        " AWS inter-region transfer within the US is billed at a flat per-GB rate with no volume tiers"
        " — report the single rate only, no tier table.\n\n"
        "2. Internet egress — 4 PB (4,000,000 GB) from us-east-1 to the public internet."
        " Note: AWS internet egress from us-east-1 uses the same US-origin tiered rates for all internet"
        " destinations worldwide (including Australia); there are no APAC- or country-specific internet egress rates.\n"
        "   Call get_price(spec={\"provider\": \"aws\", \"domain\": \"network\", \"service\": \"egress\","
        " \"source_region\": \"us-east-1\", \"destination_type\": \"internet\","
        " \"data_gb_per_month\": 4000000, \"region\": \"us-east-1\"})."
        " Show each AWS pricing tier boundary, the per-GB rate at that tier, the GB volume consumed within that tier,"
        " the cost contribution from each tier, and the blended effective per-GB rate."
        " Report the total monthly cost.\n\n"
        "Then call describe_catalog(provider=\"aws\", domain=\"network\") to check whether CloudFront pricing"
        " (service \"cdn\") is listed. If it is, call get_price(spec={\"provider\": \"aws\","
        " \"domain\": \"network\", \"service\": \"cdn\", \"data_gb_per_month\": 4000000,"
        " \"region\": \"us-east-1\"}) and report the rates returned. If cdn and egress rates are identical,"
        " state that explicitly and explain qualitatively when dedicated CloudFront pricing typically undercuts"
        " standard egress at high volumes. If CloudFront is not in the catalog at all, explain qualitatively"
        " when CloudFront pricing typically undercuts standard egress rates."
    ),
    # --- Savings Plans / Commitment Pricing ---
    "SP_CMP1": (
        "Look up pricing for a Linux r8g.large in us-east-1 across four"
        " commitment tiers: (1) on-demand, (2) 1-year Compute Savings Plan"
        " all-upfront, (3) 3-year Compute Savings Plan all-upfront, (4) 3-year"
        " EC2 Instance Savings Plan all-upfront. For each tier, use get_price"
        " with the appropriate term value and calculate: effective hourly rate,"
        " monthly cost (730 hours/month), total commitment cost over the full"
        " term, and percentage savings versus on-demand. Present the results in"
        " a table with those four columns. After the table, explicitly check"
        " whether the 3-year EC2 Instance Savings Plan effective hourly rate is"
        " lower than the 3-year Compute Savings Plan rate — if the Instance SP"
        " does not undercut the Compute SP, flag that finding and note the"
        " actual ordering."
    ),
    "SPOT1": (
        "For a Linux c6a.xlarge in us-east-1, retrieve and compare the"
        " following pricing options:\n\n"
        "1. On-demand hourly rate — use get_price with term=\"on_demand\".\n"
        "2. Spot price — call get_spot_price_history(provider=\"aws\";"
        " region=\"us-east-1\"; instance_type=\"c6a.xlarge\"). If the tool"
        " returns an upstream_failure or error, note that live spot data"
        " requires AWS credentials and include the spot row as N/A (unavailable"
        " in public-catalog mode). Do not retry.\n"
        "3. Compute Savings Plan 1yr all-upfront — term=\"compute_savings_plan_1yr\".\n"
        "4. Compute Savings Plan 3yr all-upfront — term=\"compute_savings_plan_3yr\";"
        " if this returns no data, try term=\"compute_savings_plan_3yr_all_upfront\";"
        " if still unavailable, mark the row N/A.\n\n"
        "Present results in a table: option | hourly rate | monthly cost"
        " (730 hrs) | savings vs on-demand (%). Tag all charges as recurring"
        " (monthly). After the table: (a) state the spot volatility caveat;"
        " (b) briefly explain the commitment vs interruptibility tradeoff for"
        " each option."
    ),
    "EDP1": (
        "Look up the on-demand price for a Linux c7i.2xlarge in us-east-1. Then"
        " manually compute the EDP-discounted rate by applying a 15% enterprise"
        " discount to that on-demand price (EDP is a negotiated discount absent"
        " from public APIs, so calculate it as: on-demand × 0.85). Next,"
        " retrieve the spot price for c7i.2xlarge in us-east-1 using"
        " get_spot_price_history, and note the region, AZ, and timestamp of the"
        " spot price returned. Present a three-row comparison: (1) on-demand"
        " hourly rate, (2) EDP-discounted on-demand hourly rate (on-demand ×"
        " 0.85), and (3) spot hourly rate. Tag all three as recurring (monthly,"
        " assuming 730 hours/month). Explicitly note that EDP applies to"
        " on-demand and savings plans but does NOT apply to spot pricing, so"
        " the spot price is shown as-is without any EDP reduction."
    ),
    "SPOT_FAM1": (
        "Fetch current spot prices in us-east-1 for three comparable 8 vCPU /"
        " 32 GB RAM instance types: m7i.2xlarge (Intel), m7a.2xlarge (AMD), and"
        " m7g.2xlarge (Graviton3). Call get_spot_price_history for each"
        " instance type. For each, calculate and display: (1) spot price per"
        " hour, (2) spot price per vCPU per hour (divide by 8), and (3) spot"
        " price per GB RAM per hour (divide by 32). Present results in a"
        " comparison table. Note the region, AZ, and timestamp of each spot"
        " price returned. Finally, identify which architecture offers the"
        " lowest $/vCPU and which offers the lowest $/GB RAM."
    ),
    # --- GCP Compute / Spec-Based Instance Lookup ---
    "GCS1": (
        "In GCP region us-central1, use list_instance_types to enumerate all"
        " machine types with exactly 16 vCPUs and 64 GB RAM (1:4 ratio, i.e."
        ' "standard" tier — not highmem or highcpu) across the n2, n2d, c3, c4,'
        " and e2 families. For each matching instance type found, call"
        " get_price three times to retrieve: on-demand hourly rate, 1-year CUD"
        " hourly rate, and 3-year CUD hourly rate. Present results as a table"
        " with columns: machine type, vCPUs, memory (GB), on-demand hourly,"
        " on-demand monthly (×730 h), 1yr CUD hourly, 1yr CUD monthly, 3yr CUD"
        " hourly, 3yr CUD monthly. If a family does not offer a 16vCPU/64 GB"
        " configuration, note it explicitly. All monetary values in USD."
    ),
    "AURO2": (
        "I have an Aurora PostgreSQL cluster in us-east-1: 1 writer"
        " (db.r6g.2xlarge) and 1 reader (db.r6g.large), both on-demand, with"
        " 200 GB storage. Start with describe_catalog(provider='aws',"
        " domain='database', service='aurora') to confirm the pricing structure"
        " for Aurora Standard and Aurora I/O-Optimized, including storage rates"
        " per GB and the per-million I/O request rate for Standard. Then use"
        " get_price to look up on-demand monthly compute costs for"
        " db.r6g.2xlarge and db.r6g.large (engine='aurora-postgresql',"
        " region='us-east-1'). Look up 200 GB storage cost under"
        " aurora_storage_mode='standard' and"
        " aurora_storage_mode='io_optimized'. Tag each line item as recurring"
        " (monthly). Compute total monthly cost at three I/O request volumes —"
        " 1 million, 10 million, and 100 million requests per month — for both"
        " pricing modes side by side. For Standard, apply the per-million I/O"
        " rate found in the catalog; for I/O-Optimized, apply zero I/O charge."
        " Finally, calculate the exact crossover volume (in millions of I/O"
        " requests per month) at which I/O-Optimized becomes cheaper than"
        " Standard, and state whether any of the three test volumes crosses"
        " that threshold."
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — AWS raw-SKU success (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_OK1": (
        "Our AWS Cost and Usage Report has a line item with usage type \"BoxUsage:c8g.xlarge\" "
        "billed in us-east-1. What's the on-demand hourly rate for that usage type so I can "
        "check it against what we were actually charged?"
    ),
    "RSKU_AWS_OK2": (
        "I'm reconciling our EC2 bill and one CUR row shows usage type BoxUsage:m6a.8xlarge in "
        "us-east-1. Can you pull the current public on-demand rate for that exact usage type "
        "and tell me if it lines up with $1.3824/hr?"
    ),
    "RSKU_AWS_OK3": (
        "In our billing export there's a usage type of BoxUsage:c7i.2xlarge running in "
        "us-east-1. Can you give me both the hourly rate and what that works out to per month "
        "if it runs 24/7?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — Azure no-mapping (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AZ_NF1": (
        "Our Azure billing export has a line item with meter ID "
        "00000000-0000-0000-0000-000000000000 in the eastus region, but I can't find a current "
        "rate for it anywhere. What does this meter ID actually correspond to, and what's the "
        "current price?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — Azure raw-SKU success (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AZ_OK1": (
        "I'm reconciling our Azure CUR-style usage export and one line shows meter ID "
        "3da19ca3-6007-4a29-89ea-cab10c2010ed for the eastus region. What VM SKU is that, and "
        "what's the current hourly rate?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — ambiguous match (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_AMB1": (
        "My AWS Cost and Usage Report has a line item with usage type just \"LCUUsage\" in "
        "us-east-1 — no BoxUsage prefix, and the export doesn't say which load balancer it's "
        "billing. What's the hourly rate for that?"
    ),
    "RSKU_AWS_AMB2": (
        "I'm reconciling my AWS bill and see a CUR line item with usage type "
        "\"InstanceUsage:db.r6g.large\" in us-east-1 — that's my MySQL RDS instance, right? "
        "What's the hourly rate for it?"
    ),
    "RSKU_GCP_AMB1": (
        "My GCP billing export has a Cloud KMS line item with skuId \"1017-1BAF-7159\" — what's "
        "the rate for those HSM asymmetric key versions?"
    ),
    "RSKU_GCP_AMB2": (
        "There's a GCP Cloud KMS charge on my bill for skuId \"4A51-C764-8B93\", described as "
        "\"Active Single Tenant HSM key versions (above 15000)\" — what does that cost per month?"
    ),
    "RSKU_AZ_AMB1": (
        "My Azure invoice has a Network Watcher connection-monitor charge in West US with meter "
        "ID \"ba2b4df6-e886-4cf2-9818-33f27d22b3cf\" — what's the per-unit rate for that?"
    ),
    "RSKU_AZ_AMB2": (
        "My Azure invoice has an Azure Database for MySQL Single Server (Gen5, General Purpose) "
        "compute charge in UK South with meter ID \"ace03b73-4864-4a8c-afcb-55ddf91e010e\" — "
        "what's the hourly compute rate for that vCore?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — tiered rate (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_GCP_TIER1": (
        "Our GCP billing export has a Cloud KMS line item with SKU ID 77F8-D8AF-3CCE for "
        "Autokey key-versions. Right now we're under 100 key versions a month and it's showing "
        "$0. If our key-version count grows past 100 next quarter, does the per-unit rate "
        "actually kick in at that point, or does this SKU stay free no matter how much we use?"
    ),
    "RSKU_GCP_TIER2": (
        "We're reconciling a Cloud KMS charge with SKU ID 1017-1BAF-7159 for HSM asymmetric key "
        "versions. Our HSM key usage is ramping up fast — is there a volume discount that kicks "
        "in once we cross 2000 key versions in a month, or does this SKU charge the same rate "
        "no matter how much we use?"
    ),
    "RSKU_AZ_TIER1": (
        "On our Azure invoice, meter ID 6bd64e8e-5cb9-49d3-893d-800c9b28dca3 shows up for "
        "standard outbound data transfer in southcentralus. Some months we push well past "
        "10,000 GB of egress — does the per-GB rate step down once we hit higher volumes, or is "
        "this a single flat rate no matter how much we send?"
    ),
    "RSKU_AZ_TIER2": (
        "We have meter ID 9995d93a-7d35-4d3f-9c69-7a7fea447ef4 on our Azure bill for data "
        "transfer out of mexicocentral. Our egress there is climbing past 50,000 GB some months "
        "— is there a lower per-GB rate once we cross that volume, or does this meter bill flat "
        "regardless of usage?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — batch match (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_BATCH1": (
        "I'm reconciling our AWS Cost and Usage Report for the compute team and I've got a few "
        "EC2 usage-type line items I need current on-demand rates for, all in us-east-1: "
        "BoxUsage:c8g.xlarge and BoxUsage:m6a.8xlarge. Can you price both out for me in one go?"
    ),
    "RSKU_AWS_BATCH2": (
        "Our finance team pulled these three EC2 usage-type codes off the billing export and "
        "wants a per-hour rate check for us-east-1: BoxUsage:c8g.xlarge, BoxUsage:c7i.2xlarge, "
        "and BoxUsage:m6a.8xlarge. Can you pull current on-demand pricing for all three at "
        "once?"
    ),
    "RSKU_AWS_BATCH3": (
        "Quick sanity check on two line items from our AWS bill, both us-east-1: "
        "BoxUsage:c7i.2xlarge and BoxUsage:c8g.xlarge. What's the hourly rate on each?"
    ),
    "RSKU_GCP_BATCH1": (
        "My GCP Cloud Billing export has three SKU IDs I don't recognize, all billed against "
        "us-central1: 77F8-D8AF-3CCE, 88D6-F2EE-C781, and C054-7F72-A02E. Can you tell me what "
        "each one costs?"
    ),
    "RSKU_GCP_BATCH2": (
        "I'm reconciling a GCP invoice and see SKU IDs 77F8-D8AF-3CCE and C054-7F72-A02E on the "
        "europe-west1 line items. What's the rate for each of these?"
    ),
    "RSKU_GCP_BATCH3": (
        "Two SKU IDs on my GCP bill for us-east1 that I can't match to anything internally: "
        "88D6-F2EE-C781 and C054-7F72-A02E. What am I being charged for these, and what's the "
        "per-unit rate?"
    ),
    "RSKU_AZURE_BATCH1": (
        "My Azure cost export for eastus this month has two meter IDs I need priced out: "
        "3da19ca3-6007-4a29-89ea-cab10c2010ed and cf64c470-a287-5429-8dd7-756a877824a0. Can you "
        "look both up and tell me the hourly rate for each?"
    ),
    "RSKU_AZURE_BATCH2": (
        "I'm reconciling an Azure invoice for eastus and don't recognize two of the meter IDs "
        "on it: cf64c470-a287-5429-8dd7-756a877824a0 and 93a6a529-4f49-47cb-9b1e-db9e5f23263f. "
        "Can you pull the current rate for each?"
    ),
    "RSKU_AZURE_BATCH3": (
        "Our Azure cost management export for eastus lists three distinct meterId values this "
        "cycle: 3da19ca3-6007-4a29-89ea-cab10c2010ed, cf64c470-a287-5429-8dd7-756a877824a0, and "
        "93a6a529-4f49-47cb-9b1e-db9e5f23263f. Can you get me the current price for each one so "
        "I can match them to the right line items?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — batch not-found (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_BATCH4": (
        "I've got some odd EC2 usage-type strings in our Cost and Usage Report that I don't "
        "recognize from any instance family we run: BoxUsage:zz99.999xlarge and "
        "BoxUsage:nonexistent.type, both in us-east-1. Can you check what these actually cost, "
        "or flag if they're not real instance types?"
    ),
    "RSKU_AWS_BATCH5": (
        "Two more mystery line items showed up on the export this month, both us-east-1: "
        "BoxUsage:zz99.999xlarge and CAN1-BoxUsage:totallyfake.4xlarge. Neither matches any "
        "instance type our team has ever provisioned — can you look them up and tell me what "
        "they resolve to?"
    ),
    "RSKU_AWS_BATCH6": (
        "Trying to true up last month's compute spend and three of the usage-type codes on the "
        "report don't ring a bell: BoxUsage:nonexistent.type, "
        "CAN1-BoxUsage:totallyfake.4xlarge, and BoxUsage:zz99.999xlarge, all us-east-1. Can you "
        "check current pricing for these and let me know if any of them just aren't real SKUs?"
    ),
    "RSKU_GCP_BATCH4": (
        "My GCP billing export shows SKU IDs 0000-0000-0000 and FFFF-FFFF-FFFF for us-central1, "
        "and I can't find pricing for either one anywhere. Are these even real SKUs?"
    ),
    "RSKU_GCP_BATCH5": (
        "I've got two mystery GCP SKU IDs off a europe-west1 line item: FFFF-FFFF-FFFF and "
        "1234-5678-9ABC. Can you price these out for me?"
    ),
    "RSKU_GCP_BATCH6": (
        "Three SKU IDs showed up on our GCP Cloud Billing export for us-central1 that don't "
        "match anything in our records: 0000-0000-0000, 1234-5678-9ABC, and FFFF-FFFF-FFFF. "
        "What do they cost?"
    ),
    "RSKU_AZURE_BATCH4": (
        "There are two Azure meter IDs on my eastus billing export I can't find any pricing "
        "documentation for: 00000000-0000-0000-0000-000000000000 and "
        "11111111-1111-1111-1111-111111111111. Can you check whether either one actually maps "
        "to a priced meter?"
    ),
    "RSKU_AZURE_BATCH5": (
        "Two meter IDs on our Azure eastus export don't match anything I can find: "
        "11111111-1111-1111-1111-111111111111 and 99999999-9999-9999-9999-999999999999. Can you "
        "confirm whether these are real billable meters or not?"
    ),
    "RSKU_AZURE_BATCH6": (
        "My Azure billing export for eastus has three meter IDs that look suspicious to me: "
        "00000000-0000-0000-0000-000000000000, 11111111-1111-1111-1111-111111111111, and "
        "99999999-9999-9999-9999-999999999999. Can you check all three against current pricing "
        "and tell me if any of them are legitimate?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — batch invalid SKU (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_BATCH7": (
        "I copy-pasted a couple of lines from our billing export into a spreadsheet and I think "
        "the columns got scrambled — these don't look like real SKU codes to me: \"just some "
        "random billing text\" and \"12345-not-a-sku\". Can you check whether either of these "
        "actually prices out to anything on AWS?"
    ),
    "RSKU_AWS_BATCH8": (
        "Our export tool spit out some garbage-looking entries this run — \"###invalid###\" and "
        "\"12345-not-a-sku\" — instead of proper usage-type codes. Before I file a bug with the "
        "export vendor, can you confirm these really aren't valid AWS SKUs?"
    ),
    "RSKU_AWS_BATCH9": (
        "Three rows in our cost export look totally malformed to me — \"just some random billing "
        "text\", \"###invalid###\", and \"12345-not-a-sku\" — none of them look like real AWS "
        "usage-type codes. Can you try pricing them and tell me what's going on?"
    ),
    "RSKU_GCP_BATCH7": (
        "Our finance team pasted these into the GCP cost spreadsheet as SKU references but they "
        "don't look like real SKU IDs to me: not-a-real-sku and gcp-fake-id. Can you check what "
        "they cost?"
    ),
    "RSKU_GCP_BATCH8": (
        "Someone hand-typed these SKU references into our GCP billing tracker: gcp-fake-id and "
        "???. Can you tell me what those bill at?"
    ),
    "RSKU_GCP_BATCH9": (
        "I've got three garbled entries in a GCP billing export column that's supposed to hold "
        "SKU IDs: not-a-real-sku, ???, and gcp-fake-id. What are their prices?"
    ),
    "RSKU_AZURE_BATCH7": (
        "Our billing export tool spat out a couple of garbled meter ID values for the eastus "
        "region: 'not-a-guid' and 'azure-meter-xyz'. Can you check if either of those actually "
        "resolves to anything priced?"
    ),
    "RSKU_AZURE_BATCH8": (
        "Two of the meter ID fields in our eastus Azure export got mangled somehow: "
        "'azure-meter-xyz' and '!!!bad!!!'. Can you look these up and tell me what's going on?"
    ),
    "RSKU_AZURE_BATCH9": (
        "I've got three meter ID values from an Azure eastus export that all look wrong to me: "
        "'not-a-guid', 'azure-meter-xyz', and '!!!bad!!!'. Can you check whether any of these "
        "actually price out to something real?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — batch mixed outcomes (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_BATCH10": (
        "I've got a batch of five weird line items from this month's Cost and Usage Report and "
        "I want to reconcile all of them at once: BoxUsage:c8g.xlarge, BoxUsage:zz99.999xlarge, "
        "BoxUsage:m6a.8xlarge, \"just some random billing text\", and "
        "CAN1-BoxUsage:totallyfake.4xlarge, all us-east-1. Can you price out whichever of these "
        "are real and flag anything that isn't?"
    ),
    "RSKU_GCP_BATCH10": (
        "My GCP Cloud Billing export for us-central1 has three SKU IDs I need priced all at "
        "once: 77F8-D8AF-3CCE, 0000-0000-0000, and gcp-fake-id. Can you look up all three and "
        "tell me which ones actually resolve?"
    ),
    "RSKU_AZURE_BATCH10": (
        "I need to reconcile three meter IDs from our Azure eastus export in one go: "
        "3da19ca3-6007-4a29-89ea-cab10c2010ed, 00000000-0000-0000-0000-000000000000, and "
        "'!!!bad!!!'. Can you check all three and tell me which ones are actually billable and "
        "at what rate?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — Bill of Materials (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_AWS_BOM1": (
        "My AWS Cost and Usage Report shows a cluster made up entirely of BoxUsage:c8g.xlarge "
        "line items across three different node groups — 4 of them, 2 of them, and 6 of them, "
        "all in us-east-1. Can you total up what this whole fleet costs me per month?"
    ),
    "RSKU_AWS_BOM2": (
        "I'm reconciling my AWS bill for a small web tier: the CUR shows 3x BoxUsage:c8g.xlarge "
        "for the app servers, plus we're also provisioning 500GB of gp3 EBS storage for the "
        "shared volume, all in us-east-1. What's the total monthly cost for this stack?"
    ),
    "RSKU_AWS_BOM3": (
        "We're deciding where to place a pair of m6a.8xlarge instances — the CUR usage type is "
        "BoxUsage:m6a.8xlarge. Can you compare the monthly cost of running 2 of these in "
        "us-east-1 versus eu-west-1 and tell me how much cheaper or more expensive eu-west-1 "
        "is?"
    ),
    "RSKU_AWS_BOM4": (
        "I've got a batch-processing job that only runs 200 hours a month (not 24/7) using 5x "
        "BoxUsage:c8g.xlarge instances in us-east-1. What would that actually cost me monthly "
        "given the reduced runtime?"
    ),
    "RSKU_AWS_BOM5": (
        "My billing export lists two compute line items I need to estimate together: 2x "
        "BoxUsage:c7i.2xlarge and 1x BoxUsage:z9.fake, both in us-east-1. Can you total up what "
        "this stack costs per month, and flag anything you can't price?"
    ),
    "RSKU_AWS_BOM6": (
        "I'm sizing out a small stack: 2x BoxUsage:c8g.xlarge for the app tier, plus 1x "
        "InstanceUsage:db.r6g.large for the database, both from the us-east-1 CUR. Does this "
        "whole thing estimate cleanly, or is one of these line items going to need more info "
        "from me?"
    ),
    "RSKU_AWS_BOM7": (
        "For a burst-capacity plan I want to compare 3x BoxUsage:m6a.8xlarge across us-east-1, "
        "us-west-2, and eu-west-1 — which region comes out cheapest for this instance type and "
        "by how much per month?"
    ),
    "RSKU_GCP_BOM1": (
        "My GCP Cloud Billing export has two Cloud KMS line items I want folded into a monthly "
        "estimate, both in us-central1: skuId 77F8-D8AF-3CCE (active HSM symmetric key versions "
        "for our Autokey setup) and skuId 88D6-F2EE-C781 (HSM symmetric crypto operations for "
        "Autokey). Staging runs about 60 key versions and 8,000 crypto operations a month; "
        "production runs about 400 key versions and 60,000 crypto operations a month. Can you "
        "build a combined monthly cost estimate across both environments?"
    ),
    "RSKU_GCP_BOM2": (
        "I'm putting together a monthly estimate for a small stack in us-central1: 3 "
        "n2-standard-4 Compute Engine instances running 24/7, plus the external IP charge for "
        "those same 3 VMs which shows up in my billing export as skuId C054-7F72-A02E, plus "
        "200GB of pd-ssd persistent disk. What's the total monthly cost?"
    ),
    "RSKU_GCP_BOM3": (
        "I have a Compute Engine external IP charge in my billing export — skuId C054-7F72-A02E "
        "— for 5 VMs running 24/7, and I'm deciding which region to deploy in. Can you compare "
        "the monthly cost of just that external IP charge across us-central1, europe-west4, and "
        "asia-southeast1?"
    ),
    "RSKU_GCP_BOM4": (
        "My dev team spins up 8 Compute Engine VMs with external IPs, but only during work "
        "hours — about 300 hours a month per VM, not 24/7. The billing export tags this as "
        "skuId C054-7F72-A02E in us-east1. What would the external IP portion of my monthly "
        "bill look like for those 8 dev VMs at 300 hours/month each?"
    ),
    "RSKU_GCP_BOM5": (
        "I'm building a monthly estimate for our GCP footprint in us-central1: 200 Cloud KMS "
        "Autokey HSM key versions under skuId 77F8-D8AF-3CCE, plus another line item from the "
        "billing export tagged skuId 1234-5678-90AB that I can't find documented anywhere. Can "
        "you estimate the whole bundle and flag anything that doesn't resolve?"
    ),
    "RSKU_GCP_BOM6": (
        "Our Cloud KMS Autokey usage varies a lot by environment. The billing export shows "
        "skuId 88D6-F2EE-C781 (HSM symmetric crypto operations for Autokey), all in "
        "us-central1, at roughly 2,000 operations/month in dev, 9,500 in staging, and 45,000 in "
        "production. Can you build a combined monthly cost estimate across those three "
        "environments?"
    ),
    "RSKU_GCP_BOM7": (
        "We're deciding between us-central1 and europe-west4 for a new deployment: 2 "
        "n2-standard-4 Compute Engine instances, 100GB of pd-balanced storage, and a Cloud KMS "
        "Autokey line item from our billing export — skuId 77F8-D8AF-3CCE, 150 active HSM key "
        "versions/month. Can you compare the total monthly cost of this whole stack across both "
        "regions?"
    ),
    "RSKU_AZURE_BOM1": (
        "My Azure cost export lists two Virtual Machines line items under the same Dv3 family: "
        "meterId 3da19ca3-6007-4a29-89ea-cab10c2010ed for 3 on-demand instances, and meterId "
        "cf64c470-a287-5429-8dd7-756a877824a0 for 2 Spot instances, both in eastus running "
        "24/7. Can you estimate my total monthly bill for this VM fleet?"
    ),
    "RSKU_AZURE_BOM2": (
        "I've got two rows in my Azure billing export for the same managed-disk meter, meterId "
        "93a6a529-4f49-47cb-9b1e-db9e5f23263f, in eastus — one for 12 disks attached to "
        "production and one for 2 disks in a dev environment. What's my total monthly disk "
        "spend across both?"
    ),
    "RSKU_AZURE_BOM3": (
        "I'm reconciling a mixed-cloud bill. On the Azure side there are 2 D4s v3 VMs billed "
        "under meterId 3da19ca3-6007-4a29-89ea-cab10c2010ed in eastus for our app servers, and "
        "on the AWS side we're storing 500GB of gp3 log storage in us-east-1. What's the "
        "combined monthly cost of that stack?"
    ),
    "RSKU_AZURE_BOM4": (
        "For our eastus environment I have 4 web-tier VMs I'd size as Standard_D4s_v3, plus 4 "
        "attached P10 managed data disks billed under meterId "
        "93a6a529-4f49-47cb-9b1e-db9e5f23263f — one disk per VM. Can you estimate the combined "
        "monthly cost of the VMs and their attached disks?"
    ),
    "RSKU_AZURE_BOM5": (
        "We're deciding where to run 2 D4s v3 VMs — the CUR/billing meter we're currently "
        "paying under is 3da19ca3-6007-4a29-89ea-cab10c2010ed, and today they run in eastus. "
        "Can you compare our current eastus rate for those 2 VMs against running the same 2 VMs "
        "in westus2 or centralus instead?"
    ),
    "RSKU_AZURE_BOM6": (
        "We run 3 D4s v3 Spot instances (meterId cf64c470-a287-5429-8dd7-756a877824a0) plus 3 "
        "attached P10 managed data disks (meterId 93a6a529-4f49-47cb-9b1e-db9e5f23263f) in "
        "eastus for a batch job — the VMs only run about 300 hours a month since the job shuts "
        "down nights and weekends, but the disks stay attached and billed 24/7. What would that "
        "combination cost us monthly?"
    ),
    "RSKU_AZURE_BOM7": (
        "My Azure export has 6 P10 managed disks in eastus under meterId "
        "93a6a529-4f49-47cb-9b1e-db9e5f23263f, plus one more row with meterId "
        "00000000-0000-0000-0000-badf00d00000 that I can't identify. Can you estimate my "
        "monthly disk spend and flag anything you can't price?"
    ),
    # -----------------------------------------------------------------------
    # Raw-SKU lookup — protocol edge cases (RSKU)
    # -----------------------------------------------------------------------
    "RSKU_ERR1": (
        "My AWS Cost and Usage Report has a line item for BoxUsage:c8g.xlarge but I forgot to "
        "note which region it's billed in — can you tell me what that usage type costs?"
    ),
    "RSKU_ERR2": (
        "I'm about to reconcile a stack of AWS usage-type codes from this month's billing "
        "export, but I haven't pulled the actual list together yet — can you get the batch SKU "
        "pricing check ready to go so I can just hand you the codes in a minute?"
    ),
    "RSKU_ERR3": (
        "I moved one workload over to Oracle Cloud (OCI) and my OCI bill lists a line item by "
        "its raw SKU code — can you look up its current rate the same way you did for my AWS "
        "usage-type codes?"
    ),
    "RSKU_ERR4": (
        "My monthly AWS Cost and Usage Report export lists these usage types, all billed in "
        "us-east-1 — can you price all of these in one shot: BoxUsage:x1.large, "
        "BoxUsage:x2.large, BoxUsage:x3.large, BoxUsage:x4.large, BoxUsage:x5.large, "
        "BoxUsage:x6.large, BoxUsage:x7.large, BoxUsage:x8.large, BoxUsage:x9.large, "
        "BoxUsage:x10.large, BoxUsage:x11.large, BoxUsage:x12.large, BoxUsage:x13.large, "
        "BoxUsage:x14.large, BoxUsage:x15.large, BoxUsage:x16.large, BoxUsage:x17.large, "
        "BoxUsage:x18.large, BoxUsage:x19.large, BoxUsage:x20.large, BoxUsage:x21.large, "
        "BoxUsage:x22.large, BoxUsage:x23.large, BoxUsage:x24.large, BoxUsage:x25.large, "
        "BoxUsage:x26.large, BoxUsage:x27.large, BoxUsage:x28.large, BoxUsage:x29.large, "
        "BoxUsage:x30.large?"
    ),
    "RSKU_ERR5": (
        "I have a line item on my AWS bill but the usage type column is blank — can you still "
        "figure out what it costs?"
    ),
}


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def mcp_tool_to_openai(tool) -> dict:
    """Convert an MCP tool definition to OpenAI function-calling schema."""
    return {
        "type": "function",
        "function": {
            "name": tool.name,
            "description": tool.description or "",
            "parameters": tool.inputSchema,
        },
    }


def _preview(obj, n=150) -> str:
    s = json.dumps(obj) if not isinstance(obj, str) else obj
    return s[:n] + ("…" if len(s) > n else "")


def _sanitise_tool_call_args(messages: list[dict]) -> list[dict]:
    """Ensure every tool_call arguments field in message history is valid JSON.
    vLLM will 400 on any malformed arguments string even from prior rounds.
    Also strips reasoning_content from previous assistant turns — thinking tokens
    accumulated across multiple rounds inflate the context window unnecessarily."""
    out = []
    for msg in messages:
        if msg.get("reasoning_content"):
            msg = {k: v for k, v in msg.items() if k != "reasoning_content"}
        if msg.get("role") == "assistant" and msg.get("tool_calls"):
            tcs = []
            for tc in msg["tool_calls"]:
                fn = tc.get("function", {})
                raw = fn.get("arguments", "{}")
                try:
                    json.loads(raw)
                    tcs.append(tc)
                except json.JSONDecodeError:
                    try:
                        repaired = _repair_json(_preprocess_json(raw))
                        json.loads(repaired)  # validate before accepting
                    except json.JSONDecodeError:
                        repaired = json.dumps({})
                    fixed = {**tc, "function": {**fn, "arguments": repaired}}
                    tcs.append(fixed)
            out.append({**msg, "tool_calls": tcs})
        else:
            out.append(msg)
    return out


# ---------------------------------------------------------------------------
# Loop detection
# ---------------------------------------------------------------------------


def _preprocess_json(s: str) -> str:
    """Fix common LLM JSON syntax errors before parsing."""
    # "key="value" → "key": "value"  (model mixes XML attribute and JSON syntax)
    s = re.sub(r'"(\w+)="', r'"\1": "', s)
    # "key={ → "key": {  (model uses = before a dict value)
    s = re.sub(r'"(\w+)=(\{)', r'"\1": \2', s)
    # "key=[ → "key": [  (model uses = before a list value)
    s = re.sub(r'"(\w+)=(\[)', r'"\1": \2', s)
    # {"function_name", "key": ...} → {"name": "function_name", "key": ...}
    # Model emits the function name as a bare first string instead of "name": "..."
    s = re.sub(r'^\{\s*"([^"]+)",\s*"', r'{"name": "\1", "', s.strip())
    # {"name=describe_catalog, "arguments": ...} → {"name": "describe_catalog", "arguments": ...}
    # Model uses = and omits closing quote/delimiter before continuing JSON (AZFD1/NSV5 pattern).
    s = re.sub(r'"name=([^">,\s]+),', r'"name": "\1",', s)
    return s


def _repair_json(s: str) -> str:
    """Close any unclosed braces/brackets in truncated JSON, or trim extra closing ones."""
    depth_brace = depth_bracket = 0
    in_string = escaped = False
    last_balanced_pos = len(s)
    for i, c in enumerate(s):
        if escaped:
            escaped = False
            continue
        if c == "\\" and in_string:
            escaped = True
            continue
        if c == '"':
            in_string = not in_string
            continue
        if in_string:
            continue
        if c == "{":
            depth_brace += 1
        elif c == "}":
            depth_brace -= 1
        elif c == "[":
            depth_bracket += 1
        elif c == "]":
            depth_bracket -= 1
        if depth_brace >= 0 and depth_bracket >= 0:
            last_balanced_pos = i + 1
    if depth_brace < 0 or depth_bracket < 0:
        # Extra closing token(s) — truncate to the last balanced position
        return s[:last_balanced_pos]
    return s + "]" * max(0, depth_bracket) + "}" * max(0, depth_brace)


def _extract_xml_tool_calls(content: str) -> list[dict]:
    """Parse tool calls embedded as XML in content when vLLM didn't intercept them.

    Handles multiple formats the model may produce:
    1. JSON inside <tool_call> (OpenAI definition echoed back or hermes):
         <tool_call>{"type":"function","function":{"name":X,"parameters":Y}}</tool_call>
         <tool_call>{"name":X,"arguments":Y}</tool_call>
    2. Anthropic/Claude-style <tool_calls><invoke>:
         <tool_calls><invoke name="X"><parameter name="k">v</parameter></invoke></tool_calls>
    3. Qwen3 native <tool_call><function=X><parameter=k>v</parameter></function></tool_call>
    """
    results = []
    idx = 0

    # Format 1: JSON inside <tool_call>...</tool_call> (may be truncated before closing tag)
    for m in re.finditer(r"<tool_call>\s*(\{.*?)\s*(?:</tool_call>|$)", content, re.DOTALL):
        raw = _preprocess_json(m.group(1).strip())
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            try:
                parsed = json.loads(_repair_json(raw))
            except json.JSONDecodeError:
                continue
        if parsed.get("type") == "function" and "function" in parsed:
            fn = parsed["function"]
            name = fn.get("name", "")
            args = fn.get("arguments") or fn.get("parameters") or {}
        elif "name" in parsed:
            name = parsed["name"]
            args = parsed.get("arguments") or parsed.get("parameters") or {}
        elif "function_name" in parsed:
            # {"function_name": "TOOL_NAME", "arguments": {...}} — model used "function_name" key.
            name = parsed["function_name"]
            args = parsed.get("arguments") or parsed.get("parameters") or {}
        elif "function" in parsed and isinstance(parsed["function"], str):
            # {"function": "TOOL_NAME", "arguments": {...}} — model used "function" as name key.
            name = parsed["function"]
            args = parsed.get("arguments") or parsed.get("parameters") or {}
        elif "arguments" in parsed and "provider" in (parsed.get("arguments") or {}):
            # Model omitted name but args look like a PricingSpec or catalog query.
            # Infer tool: if args have "spec" key → get_price, otherwise → describe_catalog.
            args = parsed["arguments"]
            name = "get_price" if "spec" in args else "describe_catalog"
        else:
            continue
        if not name:
            continue
        results.append(
            {
                "id": f"xml-tool-{idx}",
                "type": "function",
                "function": {
                    "name": name,
                    "arguments": json.dumps(args) if isinstance(args, dict) else str(args),
                },
            }
        )
        idx += 1

    # Format 2: Anthropic <tool_calls><invoke name="X"><parameter name="k">v</parameter></invoke>
    # (also handles truncated — only extracts complete <invoke>...</invoke> blocks)
    for m in re.finditer(r'<invoke\s+name="([^"]+)">(.*?)</invoke>', content, re.DOTALL):
        name = m.group(1)
        args = {}
        for pm in re.finditer(
            r'<parameter\s+name="([^"]+)">(.*?)</parameter>', m.group(2), re.DOTALL
        ):
            key, val = pm.group(1), pm.group(2).strip()
            try:
                args[key] = json.loads(val)
            except json.JSONDecodeError:
                args[key] = val
        results.append(
            {
                "id": f"xml-tool-{idx}",
                "type": "function",
                "function": {"name": name, "arguments": json.dumps(args)},
            }
        )
        idx += 1

    # Format 3: Qwen3 native <tool_call><function=X><parameter=k>v</parameter></function></tool_call>
    for m in re.finditer(
        r"<tool_call>\s*<function=([^>]+)>(.*?)</function>\s*</tool_call>", content, re.DOTALL
    ):
        name = m.group(1).strip()
        args = {}
        for pm in re.finditer(r"<parameter=([^>]+)>(.*?)</parameter>", m.group(2), re.DOTALL):
            key, val = pm.group(1).strip(), pm.group(2).strip()
            try:
                args[key] = json.loads(val)
            except json.JSONDecodeError:
                args[key] = val
        results.append(
            {
                "id": f"xml-tool-{idx}",
                "type": "function",
                "function": {"name": name, "arguments": json.dumps(args)},
            }
        )
        idx += 1

    # Format 4: Broken Qwen3 hybrid — JSON-like opening mixed with XML parameter syntax.
    # Seen variants:
    #   <tool_call>{"function=get_price><parameter=spec>{...}</tool_call>
    #   <tool_call>{"name=describe_catalog><parameter=provider>val</parameter>...</tool_call>
    #   <tool_call>{"function><parameter=name>func</parameter><parameter=arguments>{...}</tool_call>
    for m in re.finditer(
        r"<tool_call>\s*\{\"(?:function|name)=?([^>]*?)>\s*(.*?)\s*(?:</function>\s*)?</tool_call>",
        content,
        re.DOTALL,
    ):
        name = m.group(1).strip()
        body = m.group(2).strip()
        args: dict = {}

        # Collect all <parameter=KEY>VALUE</parameter> blocks (closing tag optional).
        params: dict = {}
        for pm in re.finditer(
            r"<parameter=([^>]+)>\s*(.*?)\s*(?:</parameter>|(?=<parameter=)|(?=</tool_call>)|$)",
            body,
            re.DOTALL,
        ):
            key = pm.group(1).strip()
            val = pm.group(2).strip()
            try:
                params[key] = json.loads(val)
            except json.JSONDecodeError:
                params[key] = val

        if not params:
            continue

        # Special case: function name embedded as <parameter=name>, args in <parameter=arguments>
        if not name and "name" in params and "arguments" in params:
            name = str(params["name"])
            raw_args = params["arguments"]
            args = raw_args if isinstance(raw_args, dict) else {}
        elif not name:
            continue
        else:
            args = params

        if not name:
            continue

        results.append(
            {
                "id": f"xml-tool-{idx}",
                "type": "function",
                "function": {"name": name, "arguments": json.dumps(args)},
            }
        )
        idx += 1

    # Format 5: Broken JSON opening followed by embedded valid JSON with "name" key.
    # e.g. <tool_call>{"function>\n{"name": "get_price", "arguments": {...}}</tool_call>
    # The outer {... is unparseable but contains a nested JSON object with the real call.
    if not results:
        for m in re.finditer(r"<tool_call>\s*\{[^{]*(\{[^{].*?)\s*(?:</tool_call>|$)", content, re.DOTALL):
            inner = m.group(1).strip()
            try:
                parsed = json.loads(inner)
            except json.JSONDecodeError:
                try:
                    parsed = json.loads(_repair_json(inner))
                except json.JSONDecodeError:
                    continue
            if "name" in parsed:
                name = parsed["name"]
                args = parsed.get("arguments") or parsed.get("parameters") or {}
                if name:
                    results.append(
                        {
                            "id": f"xml-tool-{idx}",
                            "type": "function",
                            "function": {"name": name, "arguments": json.dumps(args)},
                        }
                    )
                    idx += 1

    return results


def _call_fingerprint(tool_name: str, args: dict) -> str:
    """Canonical string for a (tool, args) pair — used to detect repeated calls."""
    return f"{tool_name}:{json.dumps(args, sort_keys=True)}"


def _loop_detected(recent_fingerprints: list[str]) -> bool:
    """Return True if any fingerprint appears twice in the recent call window."""
    seen: set[str] = set()
    for fp in recent_fingerprints:
        if fp in seen:
            return True
        seen.add(fp)
    return False


# ---------------------------------------------------------------------------
# XML hallucination detection
# ---------------------------------------------------------------------------

_XML_HALLUCINATION_PREFIXES = (
    "<tool_call",
    "<function_calls",
    '{"name":',
    '{"function_calls',
    '{"name=',
)


def _check_xml_hallucination(trace: dict) -> None:
    """Detect when the model's final answer is a raw XML/JSON tool-call string.

    Some models emit a <tool_call> or {"name": ...} block as their last message
    instead of a plain-text answer. The harness would otherwise silently mark
    these as passing. When detected, we set xml_hallucination=True and
    loop_broken=True on the trace so callers can flag them in the summary.
    """
    answer = (trace.get("final_answer") or "").strip()
    if not answer:
        return
    answer_lower = answer.lower()
    for prefix in _XML_HALLUCINATION_PREFIXES:
        if answer_lower.startswith(prefix.lower()):
            trace["xml_hallucination"] = True
            trace["loop_broken"] = True
            trace.setdefault(
                "error",
                "XML hallucination: final answer is a raw tool-call string, not a plain-text answer",
            )
            return


# ---------------------------------------------------------------------------
# Core test runner
# ---------------------------------------------------------------------------


async def run_single(
    prompt_id: str,
    prompt: str,
    mcp_session: ClientSession,
    openai_tools: list[dict],
    run_dir: Path,
) -> dict:
    print(f"\n{'=' * 64}")
    print(f"  [{prompt_id}] {prompt[:90]}")
    print(f"{'=' * 64}")

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": prompt},
    ]

    recent_fingerprints: list[str] = []  # sliding window for loop detection
    mcp_session_ok = True  # set False after a timeout; stops further MCP calls
    _call_cache: dict[str, object] = {}  # (tool, args) → result; dedup within one conversation

    trace = {
        "prompt_id": prompt_id,
        "prompt": prompt,
        "model": LLM_MODEL,
        "timestamp": datetime.utcnow().isoformat() + "Z",
        "rounds": 0,
        "tool_calls": [],
        "messages": [],
        "final_answer": None,
        "error": None,
    }

    async with httpx.AsyncClient(timeout=240.0) as client:
        for round_num in range(MAX_TOOL_ROUNDS):
            trace["rounds"] = round_num + 1

            payload = {
                "model": LLM_MODEL,
                "messages": _sanitise_tool_call_args(messages),
                "tools": openai_tools,
                "tool_choice": "auto",
                "temperature": 0.3,
                "max_tokens": 32768,
            }

            resp = None  # reset so prior round's response never leaks into error handler
            try:
                headers = {"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {}
                resp = await client.post(
                    f"{LLM_BASE_URL}/v1/chat/completions",
                    json=payload,
                    headers=headers,
                )
                resp.raise_for_status()
                data = resp.json()
            except Exception as e:
                body = ""
                try:
                    body = resp.text[:300]
                except Exception:
                    pass
                err_type = type(e).__name__
                # Type-A transient: no response received at all (empty body, connection-level
                # error). Retry this round once with a short back-off before giving up.
                if not body and not getattr(e, "response", None):
                    print(f"  TRANSIENT ({err_type}), retrying round {round_num + 1}...")
                    await asyncio.sleep(3.0)
                    try:
                        resp2 = await client.post(
                            f"{LLM_BASE_URL}/v1/chat/completions",
                            json=payload,
                            headers={"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {},
                        )
                        resp2.raise_for_status()
                        data = resp2.json()
                    except Exception as e2:
                        body2 = ""
                        try:
                            body2 = resp2.text[:300]
                        except Exception:
                            pass
                        trace["error"] = f"LLM API error (round {round_num + 1}): {type(e2).__name__}: {e2} | {body2}"
                        print(f"  ERROR (retry also failed): {e2} | {body2}")
                        break
                    # Retry succeeded — jump to the top of the loop body below
                    choice = data["choices"][0]
                    assistant_msg = choice["message"]
                    finish_reason = choice.get("finish_reason", "")
                    content = assistant_msg.get("content") or ""
                    reasoning = assistant_msg.get("reasoning_content") or ""
                    # Fall through by reassigning locals then skipping the except block
                else:
                    trace["error"] = f"LLM API error (round {round_num + 1}): {err_type}: {e} | {body}"
                    print(f"  ERROR: {err_type}: {e} | {body}")
                    break

            choice = data["choices"][0]
            assistant_msg = choice["message"]
            finish_reason = choice.get("finish_reason", "")

            # Qwen3 thinking models put the chain-of-thought in reasoning_content
            # and leave content empty until the final answer. If content is blank
            # but reasoning_content is present, we're still mid-think — treat as
            # needing more rounds. If finish_reason=stop with empty content, fall
            # back to reasoning_content so we at least capture something.
            content = assistant_msg.get("content") or ""
            reasoning = assistant_msg.get("reasoning_content") or ""

            # When vLLM's qwen3_xml parser misses tool calls, the model outputs them
            # as plain-text XML in various formats. Rescue them so the loop can still
            # execute them via MCP.
            _xml_markers = ("<tool_call>", "<tool_calls>", "<invoke ", "<function=")
            if not (assistant_msg.get("tool_calls") or []) and any(
                m in content for m in _xml_markers
            ):
                synthetic = _extract_xml_tool_calls(content)
                if synthetic:
                    stripped = re.sub(
                        r"<tool_calls?>.*?</tool_calls?>|<tool_call>.*?</tool_call>",
                        "",
                        content,
                        flags=re.DOTALL,
                    ).strip()
                    assistant_msg = {
                        **assistant_msg,
                        "content": stripped or None,
                        "tool_calls": synthetic,
                    }
                    content = stripped
                    finish_reason = "tool_calls"

            thinking_only = finish_reason == "stop" and not content.strip() and reasoning

            if thinking_only:
                # Model finished thinking but left content empty — re-prompt once.
                stub = {**assistant_msg, "content": ""}
                messages.append(stub)
                trace["messages"].append(stub)
                recovery_user = {"role": "user", "content": "Please write your final answer now."}
                messages.append(recovery_user)
                trace["messages"].append(recovery_user)
                try:
                    headers = {"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {}
                    recovery_resp = await client.post(
                        f"{LLM_BASE_URL}/v1/chat/completions",
                        json={
                            **payload,
                            "messages": _sanitise_tool_call_args(messages),
                            "tool_choice": "none",
                        },
                        headers=headers,
                    )
                    recovery_resp.raise_for_status()
                    recovery_data = recovery_resp.json()
                    if recovery_data.get("choices"):
                        rc = recovery_data["choices"][0]
                        assistant_msg = rc["message"]
                        finish_reason = rc.get("finish_reason", "")
                        content = assistant_msg.get("content") or ""
                        reasoning = assistant_msg.get("reasoning_content") or ""
                        thinking_only = not content.strip()
                except Exception:
                    pass  # fall through with thinking_only still True

            if thinking_only:
                assistant_msg = {
                    **assistant_msg,
                    "content": f"[thinking only — no final answer generated]\n\n{reasoning[:500]}",
                }

            messages.append(assistant_msg)
            trace["messages"].append(assistant_msg)

            tool_calls = assistant_msg.get("tool_calls") or []

            if not tool_calls or finish_reason in ("stop", "length"):
                final_content = assistant_msg.get("content") or reasoning
                # Strip XML tool-call blocks that slipped through (e.g. unknown JSON key formats)
                if final_content and any(m in final_content for m in _xml_markers):
                    stripped = re.sub(
                        r"<tool_calls?>.*?</tool_calls?>|<tool_call>.*?</tool_call>",
                        "",
                        final_content,
                        flags=re.DOTALL,
                    ).strip()
                    final_content = stripped or (
                        "I retrieved the requested pricing data through tool calls. "
                        "Please see the tool results above for the specific pricing "
                        "information."
                    )
                trace["final_answer"] = final_content
                if finish_reason == "length":
                    trace["error"] = (
                        f"Hit max_tokens at round {round_num + 1} — answer may be truncated"
                    )
                    print(f"  ⚠ max_tokens hit at round {round_num + 1}")
                else:
                    print(f"  ✓ Done in {round_num + 1} round(s)")
                _check_xml_hallucination(trace)
                preview = (trace["final_answer"] or "")[:300]
                print(f"  Answer preview: {preview}")
                break

            # Execute each tool call via MCP
            for tc in tool_calls:
                fn = tc["function"]
                tool_name = fn["name"]
                raw_args = fn.get("arguments") or "{}"
                try:
                    tool_args = json.loads(raw_args)
                except json.JSONDecodeError:
                    try:
                        tool_args = json.loads(_repair_json(_preprocess_json(raw_args)))
                    except json.JSONDecodeError:
                        tool_args = {}
                fn["arguments"] = json.dumps(
                    tool_args
                )  # always normalise — ensures history is clean JSON

                print(f"  → {tool_name}({_preview(tool_args, 100)})")

                # Track fingerprint in sliding window
                fp = _call_fingerprint(tool_name, tool_args)
                recent_fingerprints.append(fp)
                if len(recent_fingerprints) > LOOP_DETECT_WINDOW:
                    recent_fingerprints.pop(0)

                _cache_key = f"{tool_name}:{json.dumps(tool_args, sort_keys=True)}"
                if _cache_key in _call_cache:
                    tool_result = _call_cache[_cache_key]
                    print("     ↩ (cached — same call already made this conversation)")
                elif not mcp_session_ok:
                    tool_result = {
                        "error": "MCP session unavailable (prior timeout) — pricing unavailable"
                    }
                else:
                    try:
                        mcp_result = await asyncio.wait_for(
                            mcp_session.call_tool(tool_name, tool_args),
                            timeout=90.0,
                        )
                        # MCP returns a list of content items; join text parts
                        text_parts = [c.text for c in mcp_result.content if hasattr(c, "text")]
                        raw_text = "".join(text_parts)
                        try:
                            tool_result = json.loads(raw_text)
                        except json.JSONDecodeError:
                            tool_result = {"text": raw_text}
                        _call_cache[_cache_key] = tool_result
                    except TimeoutError:
                        tool_result = {
                            "error": "MCP tool timed out after 90s — pricing unavailable"
                        }
                        mcp_session_ok = False
                        print(
                            f"  ⚠ {tool_name} timed out — session poisoned, no more MCP calls this prompt"
                        )
                    except Exception as e:
                        tool_result = {"error": f"MCP tool error: {e}"}

                print(f"     ← {_preview(tool_result, 120)}")

                # Truncate large results to prevent context overflow
                result_str = json.dumps(tool_result)
                if len(result_str) > MAX_TOOL_RESULT_CHARS:
                    if isinstance(tool_result, dict) and "instance_types" in tool_result:
                        # Keep summary metadata, truncate the list
                        truncated = {k: v for k, v in tool_result.items() if k != "instance_types"}
                        truncated["instance_types"] = tool_result["instance_types"][:20]
                        truncated["_truncated"] = (
                            f"Showing 20 of {tool_result.get('count', '?')} results — use more specific filters"
                        )
                        tool_result = truncated
                    elif (
                        isinstance(tool_result, dict)
                        and "results" in tool_result
                        and isinstance(tool_result["results"], list)
                    ):
                        truncated = {k: v for k, v in tool_result.items() if k != "results"}
                        truncated["results"] = tool_result["results"][:20]
                        truncated["_truncated"] = (
                            f"Showing 20 of {tool_result.get('count', len(tool_result['results']))} results"
                        )
                        tool_result = truncated
                    else:
                        tool_result = {
                            "_truncated": True,
                            "preview": result_str[:MAX_TOOL_RESULT_CHARS],
                            "note": "Result too large — use more specific parameters",
                        }

                trace["tool_calls"].append(
                    {
                        "round": round_num,
                        "tool": tool_name,
                        "args": tool_args,
                        "result": tool_result,
                    }
                )

                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": tc["id"],
                        "content": json.dumps(tool_result),
                    }
                )

            # After executing all tool calls this round, check for a loop.
            # A loop is detected when the same (tool, args) fingerprint appears
            # more than once in the recent window — the model is re-querying
            # something it already tried. Force a conclusion via tool_choice=none.
            if _loop_detected(recent_fingerprints):
                print(f"  ⚠ Loop detected at round {round_num + 1} — forcing conclusion")
                trace["loop_detected"] = round_num + 1
                # Inject a user turn so the model knows to stop calling tools and
                # write its answer — without this, some models (e.g. Qwen3) emit
                # XML-formatted tool calls in the response text even with tool_choice=none.
                nudge = {
                    "role": "user",
                    "content": (
                        "STOP. Do NOT make any more tool calls. "
                        "Write your final answer as plain text ONLY — "
                        "no XML tags, no <tool_call> blocks, no JSON. "
                        "Just a normal text response summarizing what the tools returned. "
                        "If some data was unavailable, say so in plain text."
                    ),
                }
                messages.append(nudge)
                trace["messages"].append(nudge)
                try:
                    headers = {"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {}
                    loop_resp = await client.post(
                        f"{LLM_BASE_URL}/v1/chat/completions",
                        json={
                            **payload,
                            "messages": _sanitise_tool_call_args(messages),
                            "tool_choice": "none",
                        },
                        headers=headers,
                    )
                    loop_resp.raise_for_status()
                    loop_data = loop_resp.json()
                    if loop_data.get("choices"):
                        lc = loop_data["choices"][0]
                        final_msg = lc["message"]
                        content = final_msg.get("content") or ""
                        if content.strip():
                            # If the model still produced XML with tool_choice=none, try
                            # executing those calls once more before accepting the content.
                            recovered = []
                            if any(m in content for m in _xml_markers):
                                recovered = _extract_xml_tool_calls(content)
                                if recovered and mcp_session_ok:
                                    print(f"  ⚠ Loop-break response is XML — recovering {len(recovered)} tool call(s)")
                                    stripped = re.sub(
                                        r"<tool_calls?>.*?</tool_calls?>|<tool_call>.*?</tool_call>",
                                        "", content, flags=re.DOTALL,
                                    ).strip()
                                    final_msg = {**final_msg, "content": stripped or None, "tool_calls": recovered}
                                    messages.append(final_msg)
                                    trace["messages"].append(final_msg)
                                    for rtc in recovered:
                                        rfn = rtc["function"]
                                        try:
                                            rargs = json.loads(rfn.get("arguments") or "{}")
                                        except json.JSONDecodeError:
                                            rargs = {}
                                        try:
                                            r_result = await asyncio.wait_for(
                                                mcp_session.call_tool(rfn["name"], rargs), timeout=90.0
                                            )
                                            rtext = "".join(c.text for c in r_result.content if hasattr(c, "text"))
                                            try:
                                                r_tool_result = json.loads(rtext)
                                            except json.JSONDecodeError:
                                                r_tool_result = {"text": rtext}
                                        except Exception as re_:
                                            r_tool_result = {"error": str(re_)}
                                        trace["tool_calls"].append({"round": round_num, "tool": rfn["name"], "args": rargs, "result": r_tool_result})
                                        messages.append({"role": "tool", "tool_call_id": rtc["id"], "content": json.dumps(r_tool_result)})
                                    # One final forced-text call — explicitly forbid XML
                                    final_nudge_msgs = _sanitise_tool_call_args(messages) + [{
                                        "role": "user",
                                        "content": (
                                            "Now write your final answer as plain text only. "
                                            "No XML, no tool calls, no JSON. Just text."
                                        ),
                                    }]
                                    try:
                                        fr = await client.post(
                                            f"{LLM_BASE_URL}/v1/chat/completions",
                                            json={**payload, "messages": final_nudge_msgs, "tool_choice": "none"},
                                            headers={"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {},
                                        )
                                        fr.raise_for_status()
                                        fc = fr.json().get("choices", [{}])[0]
                                        content = fc.get("message", {}).get("content") or content
                                    except Exception:
                                        pass  # use whatever content we have
                                    # Strip any residual XML — model sometimes ignores tool_choice=none
                                    if any(m in content for m in _xml_markers):
                                        content = re.sub(
                                            r"<tool_calls?>.*?</tool_calls?>|<tool_call>.*?</tool_call>",
                                            "",
                                            content,
                                            flags=re.DOTALL,
                                        ).strip() or (
                                            "I retrieved the requested pricing data through tool calls. "
                                            "Please see the tool results above for the specific pricing "
                                            "information."
                                        )
                                    # Record the final text response in the trace (tool-call msg already appended above)
                                    trace["messages"].append({"role": "assistant", "content": content})
                            # Safety net: strip XML even when recovery didn't run (mcp_session unavailable)
                            if any(m in content for m in _xml_markers):
                                content = re.sub(
                                    r"<tool_calls?>.*?</tool_calls?>|<tool_call>.*?</tool_call>",
                                    "",
                                    content,
                                    flags=re.DOTALL,
                                ).strip() or (
                                    "I retrieved the requested pricing data through tool calls. "
                                    "Please see the tool results above for the specific pricing "
                                    "information."
                                )
                            # Only append final_msg when recovery did NOT fire; the recovery path
                            # already appended it at lines above to avoid a duplicate.
                            if not recovered or not mcp_session_ok:
                                messages.append(final_msg)
                                trace["messages"].append(final_msg)
                            trace["final_answer"] = content
                            trace["rounds"] = round_num + 1
                            _check_xml_hallucination(trace)
                            print(
                                f"  ✓ Loop broken — answer obtained after {round_num + 1} round(s)"
                            )
                            print(f"  Answer preview: {content[:300]}")
                            break
                except Exception as e:
                    print(f"  ✗ Loop-break request failed: {e}")
                # If the forced call also failed, continue (will re-detect next round)
                recent_fingerprints.clear()

        else:
            trace["error"] = (
                f"Hit absolute tool-round cap ({MAX_TOOL_ROUNDS}) — loop detection did not fire"
            )
            print("  ✗ Absolute cap reached")

    # Persist full trace
    out_file = run_dir / f"{prompt_id}_trace.json"
    out_file.write_text(json.dumps(trace, indent=2, default=str))
    return trace


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


async def _make_mcp_session():
    """Context manager factory — yields an initialised MCP session."""
    if MCP_URL:
        return streamablehttp_client(MCP_URL)
    server_params = StdioServerParameters(command=MCP_COMMAND, args=MCP_ARGS, env={**os.environ})
    return stdio_client(server_params)


async def _mcp_connect_with_retry(max_wait: int = 120) -> tuple:
    """Try to open an MCP session, retrying with backoff if the server is down.

    Returns (read, write) streams from a connected, initialised session context.
    Caller must use these inside `async with ClientSession(read, write) as session`.
    Raises RuntimeError if the server doesn't come back within max_wait seconds.
    """
    delay = 5
    elapsed = 0
    last_err = None
    while elapsed < max_wait:
        try:
            transport_ctx = await _make_mcp_session()
            return transport_ctx
        except Exception as e:
            last_err = e
            print(f"  ⚠ MCP unavailable ({e.__class__.__name__}), retrying in {delay}s…")
            await asyncio.sleep(delay)
            elapsed += delay
            delay = min(delay * 2, 30)
    raise RuntimeError(f"MCP did not come back within {max_wait}s: {last_err}")


async def _run_prompt_with_fresh_session(pid, prompt, openai_tools, run_dir, results, print_lock):
    """Open a fresh MCP session for a single prompt, run it, close the session.

    Retries the connection if the MCP pod has just restarted (e.g. after an OOMKill).
    """
    for attempt in range(6):
        try:
            transport_ctx = await _make_mcp_session()
            async with transport_ctx as (read, write, *_):
                async with ClientSession(read, write) as session:
                    await asyncio.wait_for(session.initialize(), timeout=30.0)
                    trace = await run_single(pid, prompt, session, openai_tools, run_dir)
            results[pid] = {
                "status": "error" if trace["error"] else "ok",
                "rounds": trace["rounds"],
                "tool_calls": len(trace["tool_calls"]),
                "tools_used": list({tc["tool"] for tc in trace["tool_calls"]}),
                "error": trace["error"],
                "answer_preview": (trace["final_answer"] or "")[:200],
                "xml_hallucination": trace.get("xml_hallucination", False),
            }
            return
        except Exception as e:
            err_str = str(e)
            is_connect_err = (
                "ConnectError" in err_str
                or "ConnectionRefused" in err_str
                or "TaskGroup" in err_str
                or "TimeoutError" in err_str
                or "timed out" in err_str.lower()
            )
            if is_connect_err and attempt < 5:
                wait = 30 * (attempt + 1)  # 30s, 60s, 90s, 120s, 150s — covers ~5min pod restart
                async with print_lock:
                    print(
                        f"  ⚠ [{pid}] MCP connect error (attempt {attempt + 1}/6), waiting {wait}s for pod restart…"
                    )
                await asyncio.sleep(wait)
            else:
                async with print_lock:
                    print(f"  FATAL [{pid}]: {e}")
                results[pid] = {
                    "status": "fatal",
                    "error": str(e),
                    "rounds": 0,
                    "tool_calls": 0,
                    "tools_used": [],
                }
                return


async def run_worker(
    items: list[tuple[str, str]],
    run_dir: "Path",
    results: dict,
    print_lock: asyncio.Lock,
) -> None:
    """Run a subset of prompts sequentially, each with its own MCP session.

    A fresh session per prompt means a timed-out tool call can't corrupt
    the connection for subsequent prompts.
    """
    # Fetch tool list once — retry if server is mid-restart.
    for attempt in range(10):
        try:
            transport_ctx = await _make_mcp_session()
            async with transport_ctx as (read, write, *_):
                async with ClientSession(read, write) as session:
                    await asyncio.wait_for(session.initialize(), timeout=30.0)
                    tools_resp = await session.list_tools()
            break
        except Exception as e:
            wait = 10 * (attempt + 1)
            print(f"  ⚠ Tool list fetch failed ({e.__class__.__name__}), retrying in {wait}s…")
            await asyncio.sleep(wait)
    else:
        raise RuntimeError("Could not fetch MCP tool list after 10 attempts")

    openai_tools = [mcp_tool_to_openai(t) for t in tools_resp.tools]

    for pid, prompt in items:
        await _run_prompt_with_fresh_session(
            pid, prompt, openai_tools, run_dir, results, print_lock
        )


async def main(ids: list[str], parallel: int = 1):
    run_dir = RESULTS_DIR / datetime.utcnow().strftime("%Y%m%d_%H%M%S")
    run_dir.mkdir(parents=True, exist_ok=True)
    print(f"Run directory: {run_dir}")

    # Select prompts
    if "all" in ids:
        selected = TEST_PROMPTS
    else:
        selected = {k: v for k, v in TEST_PROMPTS.items() if k in ids}
        missing = set(ids) - set(selected)
        if missing:
            print(f"Warning: unknown prompt IDs: {missing}")

    if not selected:
        print("No prompts selected. Use --ids C1,C2,... or --ids all")
        sys.exit(1)

    parallel = max(1, min(parallel, len(selected)))
    print(f"Running {len(selected)} prompt(s) with {parallel} worker(s): {', '.join(selected)}")

    # Distribute prompts round-robin across workers
    items = list(selected.items())
    buckets: list[list[tuple[str, str]]] = [[] for _ in range(parallel)]
    for i, item in enumerate(items):
        buckets[i % parallel].append(item)

    results: dict = {}
    print_lock = asyncio.Lock()

    await asyncio.gather(
        *[run_worker(bucket, run_dir, results, print_lock) for bucket in buckets if bucket]
    )

    summary = {
        "run_dir": str(run_dir),
        "model": LLM_MODEL,
        "timestamp": datetime.utcnow().isoformat() + "Z",
        "prompts_run": list(selected),
        "results": results,
    }

    # Save summary
    summary_file = run_dir / "summary.json"
    summary_file.write_text(json.dumps(summary, indent=2))

    print(f"\n\n{'=' * 64}")
    print(f"SUMMARY — results in {run_dir}")
    print(f"{'=' * 64}")
    for pid, r in summary["results"].items():
        if r.get("xml_hallucination"):
            print(f"  ✗ [{pid}] XML hallucination")
        else:
            status = "✓" if r["status"] == "ok" else "✗"
            print(
                f"  {status} [{pid}] {r['rounds']} round(s), "
                f"{r['tool_calls']} tool call(s)" + (f"  ERROR: {r['error']}" if r["error"] else "")
            )
    print(f"\nFull traces: {run_dir}/")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="OpenCloudCosts LLM test harness",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Configuration via .env or environment variables:\n"
            "  OCC_LLM_BASE_URL  Base URL of LLM server (e.g. http://localhost:1234)\n"
            "  OCC_LLM_MODEL     Model identifier\n"
            "  OCC_LLM_API_KEY   Optional API key\n\n"
            "Copy local-test-harness/.env.example to local-test-harness/.env to get started."
        ),
    )
    parser.add_argument(
        "--ids",
        default="all",
        help="Comma-separated prompt IDs to run (e.g. C1,C4,X2) or 'all' (default)",
    )
    parser.add_argument(
        "--llm-base-url",
        default="",
        help="Override OCC_LLM_BASE_URL — base URL of OpenAI-compatible LLM server",
    )
    parser.add_argument(
        "--model",
        default="",
        help="Override OCC_LLM_MODEL — model identifier",
    )
    parser.add_argument(
        "--api-key",
        default="",
        help="Override OCC_LLM_API_KEY — API key (if required by the server)",
    )
    parser.add_argument(
        "--parallel",
        type=int,
        default=1,
        help="Number of parallel workers (each with its own MCP session). Default: 1",
    )
    parser.add_argument(
        "--mcp-url",
        default="",
        help="Override OCC_MCP_URL — HTTP MCP endpoint (e.g. http://your-mcp-host:8080/mcp). "
        "When set, workers connect via HTTP instead of spawning a local stdio process.",
    )
    parser.add_argument(
        "--results-dir",
        default="",
        help="Override the results directory. A timestamped subdirectory will be created inside it.",
    )
    args = parser.parse_args()

    # CLI flags override env / .env
    if args.llm_base_url:
        LLM_BASE_URL = args.llm_base_url
    if args.model:
        LLM_MODEL = args.model
    if args.api_key:
        LLM_API_KEY = args.api_key
    if args.mcp_url:
        MCP_URL = args.mcp_url
    if args.results_dir:
        RESULTS_DIR = Path(args.results_dir)

    if not LLM_BASE_URL:
        print("Error: OCC_LLM_BASE_URL is not set.")
        print("Copy local-test-harness/.env.example to local-test-harness/.env and configure it.")
        sys.exit(1)
    if not LLM_MODEL:
        print("Error: OCC_LLM_MODEL is not set.")
        print("Copy local-test-harness/.env.example to local-test-harness/.env and configure it.")
        sys.exit(1)

    ids = [x.strip() for x in args.ids.split(",")]
    asyncio.run(main(ids, parallel=args.parallel))
