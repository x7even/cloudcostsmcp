# OpenCloudCosts — LLM Harness Results

**Runs:** gemma-4-26b-a4b 2026-04-23 02:28 UTC · qwen3.6-35b-a3b 2026-04-23 03:30 UTC (AS9/GC3 re-run 06:47 UTC)  
**Server:** `http://192.168.50.230:1234`  
**Tests:** 123  

## Summary

| Model | Pass | Fail | Pass Rate |
|-------|-----:|-----:|----------:|
| google/gemma-4-26b-a4b | 123 | 0 | 100.0% |
| qwen/qwen3.6-35b-a3b | 123 | 0 | 100.0% |

## Per-test matrix

| ID | Category | gemma-4-26b-a4b | qwen3.6-35b-a3b |
|:---|:---------|:---:|:---:|
| C1 | Common | ✅ (2r) | ✅ (2r) |
| C2 | Common | ✅ (2r) | ✅ (2r) |
| C3 | Common | ✅ (2r) | ✅ (2r) |
| C4 | Common | ✅ (4r) | ✅ (3r) |
| M1 | Multi-cloud | ✅ (3r) | ✅ (2r) |
| M2 | Multi-cloud | ✅ (2r) | ✅ (3r) |
| M3 | Multi-cloud | ✅ (6r) | ✅ (2r) |
| X1 | Complex BoM | ✅ (3r) | ✅ (13r) |
| X2 | Complex BoM | ✅ (7r) | ✅ (4r) |
| AS1 | AWS Simple | ✅ (2r) | ✅ (2r) |
| AS2 | AWS Simple | ✅ (2r) | ✅ (2r) |
| AS3 | AWS Simple | ✅ (2r) | ✅ (6r) |
| AS4 | AWS Simple | ✅ (4r) | ✅ (2r) |
| AS5 | AWS Simple | ✅ (4r) | ✅ (6r) |
| AS6 | AWS Simple | ✅ (3r) | ✅ (3r) |
| AS7 | AWS Simple | ✅ (2r) | ✅ (2r) |
| AS8 | AWS Simple | ✅ (9r) | ✅ (8r) |
| AS9 | AWS Simple | ✅ (4r) | ✅ (5r) |
| AS10 | AWS Simple | ✅ (3r) | ✅ (4r) |
| GS1 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS2 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS3 | GCP Simple | ✅ (2r) | ✅ (4r) |
| GS4 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS5 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS6 | GCP Simple | ✅ (3r) | ✅ (5r) |
| GS7 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS8 | GCP Simple | ✅ (2r) | ✅ (2r) |
| GS9 | GCP Simple | ✅ (2r) | ✅ (4r) |
| GS10 | GCP Simple | ✅ (2r) | ✅ (2r) |
| MP1 | AWS vs GCP | ✅ (4r) | ✅ (8r) |
| MP2 | AWS vs GCP | ✅ (3r) | ✅ (11r) |
| MP3 | AWS vs GCP | ✅ (2r) | ✅ (7r) |
| MP4 | AWS vs GCP | ✅ (3r) | ✅ (3r) |
| MP5 | AWS vs GCP | ✅ (2r) | ✅ (6r) |
| MR1 | Multi-region | ✅ (2r) | ✅ (2r) |
| MR2 | Multi-region | ✅ (2r) | ✅ (2r) |
| MR3 | Multi-region | ✅ (2r) | ✅ (2r) |
| MR4 | Multi-region | ✅ (2r) | ✅ (2r) |
| MR5 | Multi-region | ✅ (6r) | ✅ (15r) |
| CX1 | Complex TCO | ✅ (3r) | ✅ (10r) |
| CX2 | Complex TCO | ✅ (3r) | ✅ (9r) |
| CX3 | Complex TCO | ✅ (4r) | ✅ (3r) |
| CX4 | Complex TCO | ✅ (2r) | ✅ (3r) |
| CX5 | Complex TCO | ✅ (2r) | ✅ (3r) |
| CX6 | Complex TCO | ✅ (3r) | ✅ (13r) |
| CX7 | Complex TCO | ✅ (4r) | ✅ (3r) |
| CX8 | Complex TCO | ✅ (2r) | ✅ (10r) |
| CX9 | Complex TCO | ✅ (2r) | ✅ (2r) |
| CX10 | Complex TCO | ✅ (2r) | ✅ (2r) |
| CX_BOM | Other | ✅ (3r) | ✅ (18r) |
| CX11 | Complex TCO | ✅ (3r) | ✅ (2r) |
| CX12 | Complex TCO | ✅ (3r) | ✅ (2r) |
| CX13 | Complex TCO | ✅ (2r) | ✅ (2r) |
| CX14 | Complex TCO | ✅ (2r) | ✅ (8r) |
| AZX1 | Azure Complex | ✅ (2r) | ✅ (2r) |
| AZX2 | Azure Complex | ✅ (4r) | ✅ (2r) |
| AZX3 | Azure Complex | ✅ (3r) | ✅ (2r) |
| MRS1 | Multi-region Stack | ✅ (4r) | ✅ (2r) |
| MRS2 | Multi-region Stack | ✅ (2r) | ✅ (2r) |
| MRS3 | Multi-region Stack | ✅ (5r) | ✅ (6r) |
| CCR1 | Cross-Cloud | ✅ (8r) | ✅ (4r) |
| CCR2 | Cross-Cloud | ✅ (9r) | ✅ (4r) |
| CCR3 | Cross-Cloud | ✅ (7r) | ✅ (5r) |
| AZ1 | Azure Simple | ✅ (2r) | ✅ (2r) |
| AZ2 | Azure Simple | ✅ (2r) | ✅ (5r) |
| AZ3 | Azure Simple | ✅ (2r) | ✅ (2r) |
| AZ4 | Azure Simple | ✅ (2r) | ✅ (2r) |
| AZ5 | Azure Simple | ✅ (2r) | ✅ (2r) |
| AA1 | Advanced AWS | ✅ (3r) | ✅ (3r) |
| AA2 | Advanced AWS | ✅ (2r) | ✅ (5r) |
| AA3 | Advanced AWS | ✅ (2r) | ✅ (2r) |
| AA4 | Advanced AWS | ✅ (6r) | ✅ (3r) |
| AA5 | Advanced AWS | ✅ (3r) | ✅ (3r) |
| MC1 | 3-Cloud Compare | ✅ (2r) | ✅ (2r) |
| MC2 | 3-Cloud Compare | ✅ (4r) | ✅ (3r) |
| MC3 | 3-Cloud Compare | ✅ (2r) | ✅ (2r) |
| MC4 | 3-Cloud Compare | ✅ (3r) | ✅ (8r) |
| MC5 | 3-Cloud Compare | ✅ (2r) | ✅ (2r) |
| GK1 | GCP GKE | ✅ (4r) | ✅ (3r) |
| GK2 | GCP GKE | ✅ (3r) | ✅ (2r) |
| GK3 | GCP GKE | ✅ (4r) | ✅ (5r) |
| GK4 | GCP GKE | ✅ (3r) | ✅ (4r) |
| GK5 | GCP GKE | ✅ (5r) | ✅ (3r) |
| GM1 | GCP Memorystore | ✅ (3r) | ✅ (3r) |
| GM2 | GCP Memorystore | ✅ (4r) | ✅ (3r) |
| GM3 | GCP Memorystore | ✅ (3r) | ✅ (3r) |
| GM4 | GCP Memorystore | ✅ (3r) | ✅ (3r) |
| GM5 | GCP Memorystore | ✅ (3r) | ✅ (3r) |
| GB1 | GCP BigQuery | ✅ (2r) | ✅ (2r) |
| GB2 | GCP BigQuery | ✅ (6r) | ✅ (2r) |
| GB3 | GCP BigQuery | ✅ (2r) | ✅ (3r) |
| GB4 | GCP BigQuery | ✅ (5r) | ✅ (2r) |
| GB5 | GCP BigQuery | ✅ (3r) | ✅ (3r) |
| GV1 | GCP Vertex AI | ✅ (3r) | ✅ (3r) |
| GV2 | GCP Vertex AI | ✅ (4r) | ✅ (9r) |
| GV3 | GCP Vertex AI | ✅ (6r) | ✅ (3r) |
| GV4 | GCP Vertex AI | ✅ (6r) | ✅ (3r) |
| GV5 | GCP Vertex AI | ✅ (7r) | ✅ (3r) |
| GN1 | GCP Networking | ✅ (3r) | ✅ (3r) |
| GN2 | GCP Networking | ✅ (3r) | ✅ (2r) |
| GN3 | GCP Networking | ✅ (5r) | ✅ (2r) |
| GN4 | GCP Networking | ✅ (3r) | ✅ (7r) |
| GN5 | GCP Networking | ✅ (5r) | ✅ (5r) |
| GC1 | GCP Cloud Armor | ✅ (3r) | ✅ (3r) |
| GC2 | GCP Cloud Armor | ✅ (3r) | ✅ (2r) |
| GC3 | GCP Cloud Armor | ✅ (3r) | ✅ (3r) |
| GC4 | GCP Cloud Armor | ✅ (6r) | ✅ (15r) |
| GC5 | GCP Cloud Armor | ✅ (8r) | ✅ (4r) |
| GCX1 | GCP Complex | ✅ (2r) | ✅ (5r) |
| GCX2 | GCP Complex | ✅ (2r) | ✅ (6r) |
| GCX3 | GCP Complex | ✅ (5r) | ✅ (4r) |
| GCX4 | GCP Complex | ✅ (3r) | ✅ (10r) |
| GCX5 | GCP Complex | ✅ (6r) | ✅ (5r) |
| GGCS1 | GCP Cloud Storage | ✅ (2r) | ✅ (2r) |
| GGCS2 | GCP Cloud Storage | ✅ (2r) | ✅ (8r) |
| GGCS3 | GCP Cloud Storage | ✅ (2r) | ✅ (2r) |
| GGCS4 | GCP Cloud Storage | ✅ (2r) | ✅ (4r) |
| GGCS5 | GCP Cloud Storage | ✅ (4r) | ✅ (6r) |
| GSQL1 | GCP Cloud SQL | ✅ (5r) | ✅ (4r) |
| GSQL2 | GCP Cloud SQL | ✅ (5r) | ✅ (4r) |
| GSQL3 | GCP Cloud SQL | ✅ (7r) | ✅ (26r) |
| GSQL4 | GCP Cloud SQL | ✅ (5r) | ✅ (2r) |
| GSQL5 | GCP Cloud SQL | ✅ (3r) | ✅ (6r) |
| **Total** | | **123/123** | **123/123** |

---

## Model details

- **gemma-4-26b-a4b** (`google/gemma-4-26b-a4b`): 123 passed, 0 failed
- **qwen3.6-35b-a3b** (`qwen/qwen3.6-35b-a3b`): 123 passed, 0 failed

_Generated by `local-test-harness/run_matrix.py`_
