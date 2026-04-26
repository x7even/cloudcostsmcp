#!/usr/bin/env python3
"""
LLM Test Harness for OpenCloudCosts MCP

Runs test prompts through any OpenAI-compatible LLM API with the full
MCP tool-call agentic loop. Results are saved per-run for analysis.

Configuration (copy .env.example to .env and edit):
    OCC_LLM_BASE_URL   Base URL of your LLM server, e.g. http://localhost:1234
    OCC_LLM_MODEL      Model identifier as reported by the server
    OCC_LLM_API_KEY    Optional API key (for OpenAI, Anthropic-proxy, etc.)

Usage:
    uv run local-test-harness/run_tests.py
    uv run local-test-harness/run_tests.py --ids C1,C4,X2
    uv run local-test-harness/run_tests.py --ids all
    uv run local-test-harness/run_tests.py --llm-base-url http://localhost:1234 --model my-model
"""
# /// script
# requires-python = ">=3.11"
# dependencies = ["httpx", "mcp"]
# ///

import argparse
import asyncio
import json
import os
import sys
from datetime import datetime
from pathlib import Path

import httpx
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

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
PROJECT_DIR = _HARNESS_DIR.parent
MCP_COMMAND = "uv"
MCP_ARGS = ["run", "--directory", str(PROJECT_DIR), "opencloudcosts"]
# Absolute safety cap — only fires if loop detection fails to catch a pathological case.
MAX_TOOL_ROUNDS = 150
# Sliding window for loop detection: if the same (tool, args) fingerprint appears
# twice within this many consecutive calls, the model is in a loop.
LOOP_DETECT_WINDOW = 6
RESULTS_DIR = _HARNESS_DIR / "results"

SYSTEM_PROMPT = (
    "You are a cloud cost analyst. Use the available tools to look up real pricing data. "
    "Always call tools to get actual prices — never estimate or recall figures from memory. "
    "When a tool response includes a 'see_also', 'next_steps', or 'not_included' field, "
    "follow those hints by making the suggested additional tool calls before answering. "
    "Be concise and structured in your final response. "
    "If some data is unavailable (e.g. a provider requires credentials you don't have), "
    "still provide a final answer covering what you were able to retrieve, and clearly "
    "note which items are missing and why. "
    "IMPORTANT: You MUST always write your final answer as regular text in your response. "
    "Do not leave your response blank — always end with a clear written answer."
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
    "C2": (
        "I need 2TB of gp3 EBS storage in us-west-2. What will that cost per month?"
    ),
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
    "GS6": "List GCP compute instances available in asia-southeast1 with at least 16 vCPUs and show prices.",
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
        "Compare total monthly cost for a 3-tier stack with 1-year commitments across all three clouds "
        "in US regions: 3x 4-vCPU/16GB web servers + 1x 8-vCPU/32GB database + 500GB SSD. "
        "Use reserved_1yr for AWS/Azure and cud_1yr for GCP. "
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
    "AA2": (
        "I need to store 50TB in S3 Standard in us-east-1. What is the monthly storage cost?"
    ),
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
    "GM1": (
        "What does a 10GB Memorystore for Redis Basic instance cost per month in us-central1?"
    ),
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
    "GGCS3": (
        "I store 200GB in GCS Nearline in europe-west1. What is the monthly storage cost?"
    ),
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
    "AZCOS5": (
        "What would a Cosmos DB autoscale setup cost in eastus? Explain the pricing model."
    ),

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
    "AZAI1": (
        "What does Azure OpenAI GPT-4o cost per 1K input and output tokens in eastus?"
    ),
    "AZAI2": (
        "Compare Azure OpenAI GPT-4o vs GPT-3.5-Turbo pricing in eastus. "
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
    "EGR1": (
        "How much does AWS charge to transfer 1TB of data from us-east-1 to eu-west-1?"
    ),
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
    "GCPSTO3": (
        "What does GCP pd-ssd Persistent Disk cost per GB-month in us-central1?"
    ),
    "GCPDB1": (
        "What is the hourly cost for a Cloud SQL db-n1-standard-4 MySQL instance "
        "in us-central1 (zonal, no HA)?"
    ),
    "GCPDB2": (
        "Compare Cloud SQL db-n1-standard-4 MySQL: zonal vs regional (HA) pricing "
        "in us-central1. What is the monthly cost difference?"
    ),
    "GCPDB3": (
        "What does a Memorystore Redis standard-tier 8GB instance cost per hour "
        "in us-central1?"
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


# ---------------------------------------------------------------------------
# Loop detection
# ---------------------------------------------------------------------------

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
# Core test runner
# ---------------------------------------------------------------------------

async def run_single(
    prompt_id: str,
    prompt: str,
    mcp_session: ClientSession,
    openai_tools: list[dict],
    run_dir: Path,
) -> dict:
    print(f"\n{'='*64}")
    print(f"  [{prompt_id}] {prompt[:90]}")
    print(f"{'='*64}")

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": prompt},
    ]

    recent_fingerprints: list[str] = []  # sliding window for loop detection

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

    async with httpx.AsyncClient(timeout=180.0) as client:
        for round_num in range(MAX_TOOL_ROUNDS):
            trace["rounds"] = round_num + 1

            payload = {
                "model": LLM_MODEL,
                "messages": messages,
                "tools": openai_tools,
                "tool_choice": "auto",
                "temperature": 0.3,
                "max_tokens": 16384,
                "enable_thinking": False,
            }

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
                trace["error"] = f"LMStudio API error (round {round_num+1}): {e}"
                print(f"  ERROR: {e}")
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
                        json={**payload, "messages": messages, "tools": []},
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
                assistant_msg = {**assistant_msg, "content": f"[thinking only — no final answer generated]\n\n{reasoning[:500]}"}

            messages.append(assistant_msg)
            trace["messages"].append(assistant_msg)

            tool_calls = assistant_msg.get("tool_calls") or []

            if not tool_calls or finish_reason in ("stop", "length"):
                trace["final_answer"] = assistant_msg.get("content") or reasoning
                if finish_reason == "length":
                    trace["error"] = f"Hit max_tokens at round {round_num+1} — answer may be truncated"
                    print(f"  ⚠ max_tokens hit at round {round_num+1}")
                else:
                    print(f"  ✓ Done in {round_num + 1} round(s)")
                preview = (trace["final_answer"] or "")[:300]
                print(f"  Answer preview: {preview}")
                break

            # Execute each tool call via MCP
            for tc in tool_calls:
                fn = tc["function"]
                tool_name = fn["name"]
                try:
                    tool_args = json.loads(fn.get("arguments") or "{}")
                except json.JSONDecodeError:
                    tool_args = {}

                print(f"  → {tool_name}({_preview(tool_args, 100)})")

                # Track fingerprint in sliding window
                fp = _call_fingerprint(tool_name, tool_args)
                recent_fingerprints.append(fp)
                if len(recent_fingerprints) > LOOP_DETECT_WINDOW:
                    recent_fingerprints.pop(0)

                try:
                    mcp_result = await mcp_session.call_tool(tool_name, tool_args)
                    # MCP returns a list of content items; join text parts
                    text_parts = [
                        c.text for c in mcp_result.content if hasattr(c, "text")
                    ]
                    raw_text = "".join(text_parts)
                    try:
                        tool_result = json.loads(raw_text)
                    except json.JSONDecodeError:
                        tool_result = {"text": raw_text}
                except Exception as e:
                    tool_result = {"error": f"MCP tool error: {e}"}

                print(f"     ← {_preview(tool_result, 120)}")

                trace["tool_calls"].append({
                    "round": round_num,
                    "tool": tool_name,
                    "args": tool_args,
                    "result": tool_result,
                })

                messages.append({
                    "role": "tool",
                    "tool_call_id": tc["id"],
                    "content": json.dumps(tool_result),
                })

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
                        "You have enough information. Stop calling tools and write your "
                        "final answer now in plain text."
                    ),
                }
                messages.append(nudge)
                trace["messages"].append(nudge)
                try:
                    headers = {"Authorization": f"Bearer {LLM_API_KEY}"} if LLM_API_KEY else {}
                    loop_resp = await client.post(
                        f"{LLM_BASE_URL}/v1/chat/completions",
                        json={**payload, "messages": messages, "tools": [], "tool_choice": "none"},
                        headers=headers,
                    )
                    loop_resp.raise_for_status()
                    loop_data = loop_resp.json()
                    if loop_data.get("choices"):
                        lc = loop_data["choices"][0]
                        final_msg = lc["message"]
                        content = final_msg.get("content") or ""
                        if content.strip():
                            messages.append(final_msg)
                            trace["messages"].append(final_msg)
                            trace["final_answer"] = content
                            trace["rounds"] = round_num + 1
                            print(f"  ✓ Loop broken — answer obtained after {round_num + 1} round(s)")
                            print(f"  Answer preview: {content[:300]}")
                            break
                except Exception as e:
                    print(f"  ✗ Loop-break request failed: {e}")
                # If the forced call also failed, continue (will re-detect next round)
                recent_fingerprints.clear()

        else:
            trace["error"] = f"Hit absolute tool-round cap ({MAX_TOOL_ROUNDS}) — loop detection did not fire"
            print(f"  ✗ Absolute cap reached")

    # Persist full trace
    out_file = run_dir / f"{prompt_id}_trace.json"
    out_file.write_text(json.dumps(trace, indent=2, default=str))
    return trace


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

async def run_worker(
    items: list[tuple[str, str]],
    run_dir: "Path",
    results: dict,
    print_lock: asyncio.Lock,
) -> None:
    """Run a subset of prompts sequentially, each with its own MCP session."""
    server_params = StdioServerParameters(
        command=MCP_COMMAND,
        args=MCP_ARGS,
        env={**os.environ},
    )
    async with stdio_client(server_params) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools_resp = await session.list_tools()
            openai_tools = [mcp_tool_to_openai(t) for t in tools_resp.tools]
            for pid, prompt in items:
                try:
                    trace = await run_single(pid, prompt, session, openai_tools, run_dir)
                    results[pid] = {
                        "status": "error" if trace["error"] else "ok",
                        "rounds": trace["rounds"],
                        "tool_calls": len(trace["tool_calls"]),
                        "tools_used": list({tc["tool"] for tc in trace["tool_calls"]}),
                        "error": trace["error"],
                        "answer_preview": (trace["final_answer"] or "")[:200],
                    }
                except Exception as e:
                    async with print_lock:
                        print(f"  FATAL [{pid}]: {e}")
                    results[pid] = {
                        "status": "fatal",
                        "error": str(e),
                        "rounds": 0,
                        "tool_calls": 0,
                        "tools_used": [],
                    }


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

    await asyncio.gather(*[
        run_worker(bucket, run_dir, results, print_lock)
        for bucket in buckets if bucket
    ])

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

    print(f"\n\n{'='*64}")
    print(f"SUMMARY — results in {run_dir}")
    print(f"{'='*64}")
    for pid, r in summary["results"].items():
        status = "✓" if r["status"] == "ok" else "✗"
        print(
            f"  {status} [{pid}] {r['rounds']} round(s), "
            f"{r['tool_calls']} tool call(s)"
            + (f"  ERROR: {r['error']}" if r["error"] else "")
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
    args = parser.parse_args()

    # CLI flags override env / .env
    if args.llm_base_url:
        LLM_BASE_URL = args.llm_base_url
    if args.model:
        LLM_MODEL = args.model
    if args.api_key:
        LLM_API_KEY = args.api_key

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
