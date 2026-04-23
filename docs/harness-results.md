# OpenCloudCosts — LLM Harness Results

**Run date:** 2026-04-23 14:41 UTC (v0.8.4)  
**Server:** `http://192.168.50.230:1234`  
**Tests:** 123  
**Scoring:** analyse.py grounding check (hallucination + missing-data + truncation)

## Summary

| Model | Pass | Fail | Pass Rate | Notes |
|-------|-----:|-----:|----------:|-------|
| google/gemma-4-26b-a4b | 104 | 19 | 84.6% | 15 MISSING_DATA (invalid_spec), 5 HALLUCINATION |

### Failure patterns (v0.8.4 run)
- **invalid_spec / domain omitted** — GSQL1-4, GB2/4, GN3, MR5, AA3, GV4: model builds specs
  without the required `domain` field (e.g. `{"service": "cloud_sql"}` instead of
  `{"domain": "database", "service": "cloud_sql"}`). Improvement: add `domain` to the first
  example in Cloud SQL / BigQuery catalog entries.
- **invalid_spec / bad term** — C4: model invents `reserved_1yr_no_upfront` / `reserved_1yr_all_upfront`
  terms; valid values are `reserved_1yr` / `reserved_3yr`.
- **not_supported** — M3: Azure `database/mysql` unsupported (expected gap).
- **HALLUCINATION** — GC3-5: Cloud Armor per-policy/per-request amounts not grounded in tool output.

## Per-test matrix

## Per-test matrix

| ID | Category | gemma-4-26b-a4b |
|:---|:---------|:---:|
| C1 | Common | ✅ (2r) |
| C2 | Common | ✅ (2r) |
| C3 | Common | ✅ (2r) |
| C4 | Common | ✅ (3r) |
| M1 | Multi-cloud | ✅ (3r) |
| M2 | Multi-cloud | ✅ (2r) |
| M3 | Multi-cloud | ✅ (12r) |
| X1 | Complex BoM | ✅ (7r) |
| X2 | Complex BoM | ✅ (6r) |
| AS1 | AWS Simple | ✅ (2r) |
| AS2 | AWS Simple | ✅ (2r) |
| AS3 | AWS Simple | ✅ (2r) |
| AS4 | AWS Simple | ✅ (4r) |
| AS5 | AWS Simple | ✅ (5r) |
| AS6 | AWS Simple | ✅ (3r) |
| AS7 | AWS Simple | ✅ (2r) |
| AS8 | AWS Simple | ✅ (6r) |
| AS9 | AWS Simple | ✅ (5r) |
| AS10 | AWS Simple | ✅ (4r) |
| GS1 | GCP Simple | ✅ (2r) |
| GS2 | GCP Simple | ✅ (2r) |
| GS3 | GCP Simple | ✅ (2r) |
| GS4 | GCP Simple | ✅ (2r) |
| GS5 | GCP Simple | ✅ (2r) |
| GS6 | GCP Simple | ✅ (3r) |
| GS7 | GCP Simple | ✅ (2r) |
| GS8 | GCP Simple | ✅ (2r) |
| GS9 | GCP Simple | ✅ (2r) |
| GS10 | GCP Simple | ✅ (2r) |
| MP1 | AWS vs GCP | ✅ (4r) |
| MP2 | AWS vs GCP | ✅ (3r) |
| MP3 | AWS vs GCP | ✅ (2r) |
| MP4 | AWS vs GCP | ✅ (3r) |
| MP5 | AWS vs GCP | ✅ (2r) |
| MR1 | Multi-region | ✅ (2r) |
| MR2 | Multi-region | ✅ (2r) |
| MR3 | Multi-region | ✅ (2r) |
| MR4 | Multi-region | ✅ (2r) |
| MR5 | Multi-region | ✅ (7r) |
| CX1 | Complex TCO | ✅ (3r) |
| CX2 | Complex TCO | ✅ (3r) |
| CX3 | Complex TCO | ✅ (4r) |
| CX4 | Complex TCO | ✅ (2r) |
| CX5 | Complex TCO | ✅ (2r) |
| CX6 | Complex TCO | ✅ (3r) |
| CX7 | Complex TCO | ✅ (4r) |
| CX8 | Complex TCO | ✅ (3r) |
| CX9 | Complex TCO | ✅ (2r) |
| CX10 | Complex TCO | ✅ (2r) |
| CX_BOM | Other | ✅ (12r) |
| CX11 | Complex TCO | ✅ (2r) |
| CX12 | Complex TCO | ✅ (3r) |
| CX13 | Complex TCO | ✅ (2r) |
| CX14 | Complex TCO | ✅ (2r) |
| AZX1 | Azure Complex | ✅ (3r) |
| AZX2 | Azure Complex | ✅ (3r) |
| AZX3 | Azure Complex | ✅ (2r) |
| MRS1 | Multi-region Stack | ✅ (2r) |
| MRS2 | Multi-region Stack | ✅ (2r) |
| MRS3 | Multi-region Stack | ✅ (2r) |
| CCR1 | Cross-Cloud | ✅ (4r) |
| CCR2 | Cross-Cloud | ✅ (5r) |
| CCR3 | Cross-Cloud | ✅ (3r) |
| AZ1 | Azure Simple | ✅ (2r) |
| AZ2 | Azure Simple | ✅ (2r) |
| AZ3 | Azure Simple | ✅ (2r) |
| AZ4 | Azure Simple | ✅ (2r) |
| AZ5 | Azure Simple | ✅ (2r) |
| AA1 | Advanced AWS | ✅ (3r) |
| AA2 | Advanced AWS | ✅ (5r) |
| AA3 | Advanced AWS | ✅ (4r) |
| AA4 | Advanced AWS | ✅ (4r) |
| AA5 | Advanced AWS | ✅ (3r) |
| MC1 | 3-Cloud Compare | ✅ (2r) |
| MC2 | 3-Cloud Compare | ✅ (3r) |
| MC3 | 3-Cloud Compare | ✅ (2r) |
| MC4 | 3-Cloud Compare | ✅ (3r) |
| MC5 | 3-Cloud Compare | ✅ (2r) |
| GK1 | GCP GKE | ✅ (4r) |
| GK2 | GCP GKE | ✅ (3r) |
| GK3 | GCP GKE | ✅ (3r) |
| GK4 | GCP GKE | ✅ (3r) |
| GK5 | GCP GKE | ✅ (3r) |
| GM1 | GCP Memorystore | ✅ (3r) |
| GM2 | GCP Memorystore | ✅ (3r) |
| GM3 | GCP Memorystore | ✅ (3r) |
| GM4 | GCP Memorystore | ✅ (3r) |
| GM5 | GCP Memorystore | ✅ (3r) |
| GB1 | GCP BigQuery | ✅ (2r) |
| GB2 | GCP BigQuery | ✅ (5r) |
| GB3 | GCP BigQuery | ✅ (2r) |
| GB4 | GCP BigQuery | ✅ (5r) |
| GB5 | GCP BigQuery | ✅ (3r) |
| GV1 | GCP Vertex AI | ✅ (6r) |
| GV2 | GCP Vertex AI | ✅ (4r) |
| GV3 | GCP Vertex AI | ✅ (3r) |
| GV4 | GCP Vertex AI | ✅ (6r) |
| GV5 | GCP Vertex AI | ✅ (3r) |
| GN1 | GCP Networking | ✅ (3r) |
| GN2 | GCP Networking | ✅ (3r) |
| GN3 | GCP Networking | ✅ (5r) |
| GN4 | GCP Networking | ✅ (3r) |
| GN5 | GCP Networking | ✅ (6r) |
| GC1 | GCP Cloud Armor | ✅ (3r) |
| GC2 | GCP Cloud Armor | ✅ (3r) |
| GC3 | GCP Cloud Armor | ✅ (4r) |
| GC4 | GCP Cloud Armor | ✅ (5r) |
| GC5 | GCP Cloud Armor | ✅ (6r) |
| GCX1 | GCP Complex | ✅ (3r) |
| GCX2 | GCP Complex | ✅ (2r) |
| GCX3 | GCP Complex | ✅ (5r) |
| GCX4 | GCP Complex | ✅ (3r) |
| GCX5 | GCP Complex | ✅ (5r) |
| GGCS1 | GCP Cloud Storage | ✅ (2r) |
| GGCS2 | GCP Cloud Storage | ✅ (2r) |
| GGCS3 | GCP Cloud Storage | ✅ (2r) |
| GGCS4 | GCP Cloud Storage | ✅ (2r) |
| GGCS5 | GCP Cloud Storage | ✅ (4r) |
| GSQL1 | GCP Cloud SQL | ✅ (5r) |
| GSQL2 | GCP Cloud SQL | ✅ (4r) |
| GSQL3 | GCP Cloud SQL | ✅ (7r) |
| GSQL4 | GCP Cloud SQL | ✅ (5r) |
| GSQL5 | GCP Cloud SQL | ✅ (3r) |
| **Total** | | **123/123** |

---

## Model details

- **gemma-4-26b-a4b** (`google/gemma-4-26b-a4b`): 123 passed, 0 failed

_Generated by `local-test-harness/run_matrix.py` on 2026-04-23 14:41 UTC_
