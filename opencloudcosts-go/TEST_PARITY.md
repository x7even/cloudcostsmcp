# Test Parity Workplan

## Summary

| Domain | Implemented | Invalid/Skip | Failed | Total |
|---|---|---|---|---|
| aws-pricing | 2 | 2 | 0 | 4 |
| aws-bulk | 3 | 0 | 0 | 3 |
| aws-finops | 2 | 0 | 0 | 2 |
| azure | 8 | 0 | 0 | 8 |
| azure-cosmosdb | 5 | 0 | 0 | 5 |
| azure-monitor | 5 | 0 | 0 | 5 |
| azure-frontdoor | 4 | 0 | 0 | 4 |
| gcp-networking | 9 | 0 | 0 | 9 |
| gcp-armor | 4 | 0 | 0 | 4 |
| gcp-compute | 4 | 0 | 0 | 4 |
| gcp-compute-sud | 11 | 0 | 0 | 11 |
| gcp-vertex-ai | 5 | 0 | 0 | 5 |
| gcp-gke | 5 | 0 | 0 | 5 |
| gcp-database | 3 | 0 | 0 | 3 |
| tools-lookup | 5 | 3 | 0 | 8 |
| tools-lookup-no-results | 4 | 0 | 0 | 4 |
| provider-contract | 2 | 0 | 0 | 2 |
| provider-term-invariants | 11 | 0 | 0 | 11 |
| models | 4 | 0 | 0 | 4 |
| cache | 2 | 0 | 0 | 2 |
| bom | 1 | 1 | 0 | 2 |
| egress | 3 | 0 | 0 | 3 |
| **Totals** | **102** | **6** | **0** | **108** |

## Implemented Tests

### aws-pricing

| Test Name | Commit | Notes |
|---|---|---|
| TestExtractReservedPrice_AllUpfront_Normalised | 919ef93 | Tests that extractReservedPrice correctly normalises a $560 All-Upfront 1yr reserved instance to ~$0.0639/hr (560/8760) instead of returning $0/hr. The Go implementation already handles this correctly via upfront / (8760 * years); the test prevents regression. Test fixture allUpfrontJSON added to aws_pricing_test.go. |
| TestNetworkEgress_Internet_TieredBreakdown | 40d3f73 | Test revealed a real bug: GetNetworkPrice checked 'p.pricingClient != nil' and skipped GetProducts entirely when in bulkFallback mode, so the transferType='InterRegion Outbound' filter was never exercised without live credentials. Fixed by changing the guard to 'p.pricingClient != nil || p.bulkFallback'. Test uses httptest.NewServer to serve a minimal AWSDataTransfer bulk JSON; asserts the result has no fallback=true attribute and price matches fixture ($0.02/GB). |

### aws-bulk

| Test Name | Commit | Notes |
|---|---|---|
| TestGetProductsBulk_ReservedTermsCollected | ce495cb | Verifies that Reserved terms are collected from the bulk JSON streaming pass and included in the per-SKU output. Builds a fixture with only Reserved terms (empty OnDemand), calls getProductsBulk, unmarshals the result as parsedSKU, and asserts Terms.Reserved is non-empty with keys prefixed by the SKU. |
| TestGetProductsBulk_ProductFamilyFilter | 4a44753 | Verifies that a productFamily filter matches the top-level productFamily field on the product (not inside the attributes map). Uses a two-product fixture (Storage + Compute Instance) and filters for productFamily=Storage, asserting only STORAGE_SKU is returned. Exercises matchesProduct which folds productFamily into the lookup map. |
| TestGetProductsBulk_MalformedJSON | c29ebc2 | Verifies that malformed bulk JSON (truncated/invalid JSON served via httptest.Server) causes getProductsBulk to return a non-nil error without panicking. The test asserts err != nil; Go test runner catches any panics automatically. |

### aws-finops

| Test Name | Commit | Notes |
|---|---|---|
| TestGetDiscountSummary_EmptyLists | 8ee6a1b | Uses three httptest.NewServer instances to mock Savings Plans (REST/JSON), Cost Explorer (awsjson1.1), and EC2 (EC2Query/XML) APIs. Verifies GetDiscountSummary returns sp_count=0 and ri_count=0 with no error when all APIs return empty data. Corresponds to Python test_get_discount_summary_empty in test_phase2.py. |
| TestGetEffectivePrice_NotConfigured | b21e0b0 | Verifies GetEffectivePrice returns errCENotConfigured (wrapping providers.ErrNotSupported) with nil result when ceClient is nil. Checks that the error message contains 'Cost Explorer' and 'OCC_AWS_ENABLE_COST_EXPLORER' hint. Corresponds to Python test_get_discount_summary_no_auth which expects NotConfiguredError when aws_enable_cost_explorer=False; the Go provider returns an error (not a structured map), which the tool layer converts to {"error":"not_configured"}. |

### azure

| Test Name | Commit | Notes |
|---|---|---|
| TestGetComputePrice_ZeroPriceFiltered | 069c841 | Verifies that azureRetailItem entries with retailPrice=0 are excluded from GetComputePrice results via itemToPrice returning nil for zero-price items. |
| TestGetComputePrice_LinuxExcludesWindowsSKUs | ec70462 | Verifies that when os=Linux is requested, items with productName containing 'Windows' are filtered out. Uses two mock items (Linux and Windows productName), asserts only the Linux item is returned. |
| TestGetComputePrice_WindowsExcludesLinuxSKUs | 1b9192c | Verifies that when os=Windows is requested, only items with productName containing 'Windows' are returned. Uses two mock items, asserts only the Windows item at price 0.752 is returned. |
| TestGetComputePrice_SortedCheapestFirst | 919b5e7 | Verifies that GetComputePrice results are sorted ascending by price. Mock server returns expensive item first (0.384), cheap item second (0.300); asserts results are reversed (cheap first). |
| TestGetSQLPrice_CacheHit | 21f0a15 | Uses countingServer to verify that a second call to GetSQLPrice with identical arguments hits the cache and does not make a second HTTP request (call count remains 1). |
| TestGetEgressPrice_SwedenCentral_IsZone1 | 7ca1937 | Verifies that swedencentral (not in azureEgressZone map) defaults to zone1, and uses the zone1 static fallback rate of $0.087/GB. Uses empty mock response to force static fallback. |
| TestEgressServiceField_BothDomains | 394644b | Verifies egress pricing via GetPrice works for both EgressPricingSpec (PricingDomainInterRegionEgress) and NetworkPricingSpec (PricingDomainNetwork, service=egress), and that both return prices with service='egress'. |
| TestGetFrontDoorPrice_Zone2 | b83ffac | Verifies that southeastasia maps to Zone 2 in cdnZone and returns the Zone 2 static fallback rate ($0.160/GB), which differs from the Zone 1 rate ($0.0825/GB). Checks the cdn_zone attribute and price value. |

### gcp-networking

| Test Name | Commit | Notes |
|---|---|---|
| TestPriceNetworkLB_RateFromSKU | 5344eab | Uses httptest.NewServer with fake LB SKUs. Asserts PricePerUnit=0.008 on the forwarding_rule component price and breakdown['fallback'] != true. Go equivalent of Python test_lb_rule_rate_from_sku. |
| TestPriceNetworkLB_CostMath | dcccba6 | Asserts monthly_rule_cost, monthly_data_cost, and monthly_total breakdown values match expected float64 amounts within 1e-9 tolerance. Verifies monthly LB cost arithmetic (2 rules x $0.008 x 730 hr + 100 GB data). |
| TestPriceNetworkLB_Fallback | dda701e | Server returns HTTP 500; test asserts fallback=true and PricePerUnit=0.008 from the hardcoded rate. Verifies that when API returns HTTP 500, LB pricing uses hardcoded fallback rates and sets breakdown['fallback']=true. |
| TestPriceNetworkCDN_EgressRateFromSKU | 718ee81 | Fixed an off-by-one bug during development (':egress' is 7 chars not 6 for SKUID suffix matching). Final assertion uses pr.SKUID[len-7:] == ':egress'. Verifies CDN egress rate is read from SKU ($0.02/GB). |
| TestPriceNetworkCDN_CostMath | da90da3 | Asserts monthly_egress_cost=20.0 (1000GB x $0.02), monthly_cache_fill_cost=5.0 (500GB x $0.01), and monthly_total=25.0. Verifies CDN monthly cost = egress_cost + cache_fill_cost. |
| TestPriceNetworkNAT_GatewayRateFromSKU | cb3826e | Matches gateway price by SKUID suffix ':gateway' (8 chars). Asserts PricePerUnit=0.044 and fallback is not set. Verifies NAT gateway hourly rate is extracted from SKU ($0.044/hr). |
| TestPriceNetworkNAT_CostMath | f0166e3 | Asserts monthly_gateway_cost=32.12 (1 gateway x $0.044 x 730hr), monthly_data_cost=4.5 (100GB x $0.045), and monthly_total=36.62. Verifies NAT monthly cost = gateway_cost + data_cost. |
| TestGetEgressPrice_InternetAmericas | 71c2570 | Uses DestinationType='internet' and Region='us-central1'. Asserts PricePerUnit=0.08 from gcpInternetEgressBaseRate['americas'] constant. |
| TestGetEgressPrice_CrossContinent | 39fd0fe | Uses DestinationType='intra_gcp', SourceRegion='us-central1', DestinationRegion='europe-west1'. Asserts PricePerUnit=0.08 from gcpIntraEgressRate['cross'] constant. |

### gcp-armor

| Test Name | Commit | Notes |
|---|---|---|
| TestPriceNetworkArmor_CostMath | 0138db3 | Exercises priceNetworkArmor with 3 policies at $0.75 and 50M requests at $0.75/M via httptest.Server. Verifies monthly_policy_cost=$2.25, monthly_request_cost=$37.50, monthly_total=$39.75 in the breakdown map. |
| TestPriceNetworkArmor_FallbackOnFetchError | 0138db3 | Exercises the fallback path in priceNetworkArmor by serving HTTP 500 from the test server. Verifies breakdown['fallback']=true and that both policy and request NormalizedPrice entries carry the hardcoded $0.75 fallback rate. |
| TestPriceObservability_TieredCostSmall | b2a3987 | Exercises priceObservability with a $0.258/MiB monitoring SKU served via httptest.Server and ingestion_mib=200. Verifies breakdown['estimated_monthly_cost'] equals '$12.9000/month' (50 billable MiB x $0.258), matching Go's fmt.Sprintf('$%.4f/month', cost) format. |
| TestPriceObservability_TieredCostLarge | f309e91 | Exercises priceObservability with ingestion_mib=150000 using forced fallback (HTTP 500). Verifies tier-boundary math: tier1=100000x$0.258=$25800, tier2=49850x$0.151=$7527.35, total=$33327.35, and fallback=true. |
| TestPriceObservability_FallbackOnFetchError | 6fa9041 | Exercises priceObservability with ingestion_mib=0 and HTTP 500 to force fallback. Verifies breakdown['fallback']=true, free_tier_mib=150.0, and all three tier rate strings contain their expected values (0.258, 0.151, 0.062). |

### gcp-compute

| Test Name | Commit | Notes |
|---|---|---|
| TestGetComputePrice_CUD1Yr_NumericPrice | 7f88346 | Tests GetComputePrice with PricingTermCUD1Yr using Commit1Yr SKU descriptions. Verifies price is positive, equals 4x$0.019560 + 16x$0.002626 = $0.120256, and pricing term is set to cud_1yr. Uses httptest.NewServer with canned CUD SKUs. |
| TestGetStoragePrice_GCS_AllTiers | 2018cd0 | Tests GetStoragePrice returns non-empty results for all four GCS tiers: standard ($0.020), nearline ($0.010), coldline ($0.004), archive ($0.0012). Uses a single httptest.Server with four SKUs matching the gcsStorageClasses map. Verifies price and unit for each tier. |
| TestGetStoragePrice_PricingOrder | 12283df | Verifies GCS tier pricing order invariant: archive <= coldline <= nearline <= standard. Uses httptest.NewServer with realistic GCS pricing. Tests both the dominance of standard over all cheaper tiers and the full ordering chain. |
| TestGetComputePrice_CustomMachineType | 5c96cc2 | Tests that n2-standard-200 (not in GCPInstanceSpecs table) falls back to naming-convention parsing (200 vCPU, 800 GB RAM) and returns 200x$0.031611 + 800x$0.004237 = $9.7118/hr. Verifies instanceType attribute is set correctly. |

### gcp-compute-sud

New capability (no Python equivalent — SUD is a Go-only addition). All 11 tests added in commit 30d5c54.

| Test Name | Commit | Notes |
|---|---|---|
| TestGetComputePrice_SUD_EligibleN1_ReturnsTerm | 30d5c54 | Verifies that n1-standard-4 with term=sud returns a non-empty result with Term==PricingTermSUD. Mock server serves on-demand CPU and RAM SKUs; gcpSUDPrice fetches these and derives SUD from them. |
| TestGetComputePrice_SUD_BlendedRateMath | 30d5c54 | Asserts blended SUD PricePerUnit = on_demand_total × 0.70 within 1e-4 tolerance. For n1-standard-4 (4 vCPU × $0.031611 + 15 GB × $0.004237 = $0.190/hr on-demand), blended = $0.133/hr. |
| TestGetComputePrice_SUD_TermLabelIsSUD | 30d5c54 | Asserts Term==models.PricingTermSUD, NOT on_demand or any other string. Guards against mis-labelling the SUD rate as the on-demand rate. |
| TestGetComputePrice_SUD_AttributesComplete | 30d5c54 | Asserts all required attribute keys present: sud_tier_0 through sud_tier_3, sud_blended_factor (=="0.700"), sud_discount_pct (=="30.0"), usage_assumption, sud_rate_source (contains "catalog"), note. |
| TestGetComputePrice_SUD_TierRatesDescending | 30d5c54 | Parses dollar amounts from tier attribute strings and asserts tier_0 > tier_1 > tier_2 > tier_3 > 0. Validates the published 0%/20%/40%/60% discount schedule produces strictly decreasing rates. |
| TestGetComputePrice_SUD_IneligibleA2_ReturnsEmpty | 30d5c54 | Verifies that a2-highgpu-1g with term=sud returns an empty slice and no error. GPU families do not qualify; returning empty (not error) lets callers distinguish "not eligible" from "pricing failure". |
| TestGetComputePrice_SUD_IneligibleG2_ReturnsEmpty | 30d5c54 | Same ineligibility check for g2-standard-4. GPU families (a2, a3, g2) are all excluded from SUD. |
| TestGetComputePrice_PriceLadder_SpotLtCUD3LtCUD1LtSUDLtOnDemand | 30d5c54 | Pricing ladder invariant for n2-standard-4: spot < cud_3yr < cud_1yr < sud < on_demand. Mock server serves SKUs for all five terms; asserts the strict ordering holds. SUD must always undercut on_demand but cost more than a 1-year CUD commitment. |
| TestGetComputePrice_OnDemand_SUDHintPresent | 30d5c54 | Verifies on_demand result for n1-standard-4 carries Attributes["sud_eligible"]=="true" and a non-empty "sud_blended_rate_at_100pct" hint mentioning "30". Allows callers to discover SUD eligibility from any on_demand response. |
| TestGetComputePrice_OnDemand_SUDHintAbsentForGPU | 30d5c54 | Verifies a2-highgpu-1g on_demand result does NOT carry sud_eligible="true". GPU instances must not mislead callers into thinking they qualify for SUD. |
| TestSUDEligible_Families | 30d5c54 | Table-driven test of utils.SUDEligible(): n1,n2,n2d,e2,c2,c2d,c3,t2d,t2a,m1,m2,m3 → true; a2,a3,g2 → false. Also verifies case-insensitivity (N1 → true). |

### gcp-gke

| Test Name | Commit | Notes |
|---|---|---|
| TestGetGKEPrice_AutopilotNegativeVCPU | 429b0c2 | Added input validation guard in gkeAutopilotPrice rejecting vcpu < 0 with an error. Test verifies the guard fires and returns a non-nil error. Bug fix was required since the original code produced negative costs silently. |
| TestGetGKEPrice_AutopilotNegativeMemory | f63cff7 | Same guard added in gkeAutopilotPrice also rejects memoryGB < 0. Test verifies the error is returned. |
| TestGetGKEPrice_InvalidMode | f63cff7 | Unknown mode falls through to the standard branch (not autopilot) in Go, matching documented Python provider behaviour where the tool layer validates mode. Test asserts no panic, non-nil breakdown with 'mode' key, and at least one price returned via standard fallback. |
| TestGetMemstorePrice_StandardMoreExpensiveThanBasic | 6e5978e | Exercises getMemstorePrice twice for 6 GB capacity (m3 tier) with controlled SKU rates: basic $0.049/GiB-hr, standard $0.065/GiB-hr. Asserts standard PricePerUnit >= basic PricePerUnit. Validates the SKU-matching and tier-selection code paths. |
| TestGetBigQueryPrice_FreeTierNote | de517aa | The 'note' field already exists in getBigQueryPrice breakdown (gcp_ai.go:483). Test verifies the breakdown map contains a 'note' string key that mentions '1 TiB' free query threshold. Uses manual substring search to avoid needing 'strings' import. |

### gcp-database

| Test Name | Commit | Notes |
|---|---|---|
| TestGetCloudSQLPrice_PricingMathDecomposed | 8339049 | Verifies that the Cloud SQL price for db-n1-standard-8 (8 vCPU, 30 GB) equals cpu_count*cpu_rate + mem_gb*ram_rate. GCP encodes this total into a single SKU description; the test serves that SKU with the computed nanos value and asserts the returned PricePerUnit matches the decomposed formula within floating-point tolerance. |
| TestGetCloudSQLPrice_EngineNormalization | f451345 | Verifies that 'postgres', 'postgresql', and 'pg' aliases all resolve to the canonical 'PostgreSQL' engine string (via the cloudSQLEngineNames map) and successfully match the same PostgreSQL SKU description. Uses table-driven sub-tests with independent providers to avoid cache interference across aliases. |
| TestGetCloudSQLPrice_HAMultiplier | 9b56075 | Verifies that Regional (HA) Cloud SQL pricing is greater than Zonal pricing for the same instance type. Serves both Zonal ($0.2702/hr) and Regional ($0.5404/hr, ~2x) SKUs from a single httptest.Server and asserts the regional price exceeds the zonal price. Also checks that the ha attribute is false/true on each result. |

### provider-contract

| Test Name | Commit | Notes |
|---|---|---|
| TestPricingResult_AuthGating | 243b7d7 | Verifies that PricingResult JSON serialization omits contracted_prices and effective_price fields when auth_available=false (via omitempty on nil fields), and includes them when auth_available=true and the fields are populated. Two subtests: no_auth_omits_contracted_and_effective and with_auth_includes_contracted_and_effective. Mirrors Python test_pricing_result_summary_with_no_auth / test_pricing_result_summary_with_auth. |
| TestAllProvidersImplementInterface | 40d3f73 | Constructs minimal instances of AWS (via internal struct to avoid credential probe), Azure (via NewProvider), and GCP (via NewProvider) providers, assigns each to a providers.Provider interface variable, and asserts correct Name(), non-empty DefaultRegion(), and non-empty MajorRegions(). Complements the existing per-package compile-time guards with a runtime assertion covering all three providers in one test. |

### models

| Test Name | Commit | Notes |
|---|---|---|
| TestNormalizedPrice_ZeroValueIsValid | 052c5a9 | Verifies that a NormalizedPrice with PricePerUnit=0.0 is a valid Go struct (no panic), and that MonthlyCost() and HourlyCost() both return 0.0, matching the free-tier behaviour in Python's Decimal model. |
| TestPricingSpec_RequiredFields | 6b217af | Tests that UnmarshalPricingSpec returns an error when the 'domain' field is absent or empty. Go does not validate 'provider' as required (no Pydantic equivalent), so the test covers the domain-required constraint that Go actually enforces via its discriminated-union dispatcher. |
| TestPricingResult_JSONRoundtrip | 243b7d7 | Marshals a fully-populated NormalizedPrice (including FetchedAt, SourceURL, CacheAgeSeconds, Attributes) to JSON and unmarshals it back, asserting all fields survive the round-trip without loss. |
| TestPricingTerm_AllValues | 58fb699 | Iterates all 17 PricingTerm constants, checking that each string value is non-empty and that no two constants share the same string representation (uniqueness guard). |

### cache

| Test Name | Commit | Notes |
|---|---|---|
| TestCacheManager_ExpiredEntryReturnsNotFound | 42e216d | Verifies that Get() after TTL expiry returns both ok=false AND a nil byte slice (not stale data). The existing TestTTLExpiry only checked ok=false but discarded the value; this test adds the nil-value assertion that directly matches the backlog description of 'not stale data'. |
| TestCacheManager_ConcurrentWrites | 3b16089 | Launches 40 goroutines each writing to a unique key, then verifies every key holds exactly its writer's value with no cross-contamination. Run with -race flag; all 13 cache tests pass cleanly. |

### bom

| Test Name | Commit | Notes |
|---|---|---|
| TestEstimateBOM_PartialFailure | 7bdf293 | When one item fails GetPrice and another succeeds, HandleEstimateBOM returns the successful line_item in 'line_items' and sets the 'errors' field to describe the failure — no top-level 'error' key. Test uses a mockProvider whose getPriceFunc returns success on the first call and an error on the second. Verifies absence of top-level error, exactly 1 line item, and a non-nil errors slice. |

### egress

| Test Name | Commit | Notes |
|---|---|---|
| TestEgressTiers_FirstTierBreakpoint | 922d379 | Tests 15000 GB crossing the 10 TB boundary: 100 GB free + 10240 GB at $0.090 + 4660 GB at $0.085. Verifies tier[0] rate=0.000, tier[1] rate=0.090 with 10240 GB volume, tier[2] rate=0.085 with 4660 GB volume. Mirrors Python test_aws_internet_egress_5000gb_crosses_tiers extended to actually cross the 10 TB boundary. |
| TestEgressTiers_ZeroBytes | 9818b8c | Tests that zero data_gb with the full 6-tier AWS tier table returns total_cost=0.0000, blended_rate_per_gb=0.0000, data_gb=0.0, and a non-nil empty tiers slice. Mirrors Python test_aws_egress_zero_gb_returns_rate_no_error and test_gcp_egress_zero_gb_no_error. |
| TestEgressTiers_ExactBreakpoint | cee89af | Tests exactly 10340 GB (100 GB free + 10240 GB paid = precise end of the first paid tier). Verifies exactly 2 tiers are emitted (free + first paid), the $0.085 tier does not appear, and total cost equals exactly 10240*0.090=$921.60. Validates that the boundary condition does not over-allocate or bleed into the next tier. |

## Invalid / Not Applicable

### aws-pricing

| Test Name | Reason |
|---|---|
| TestGetComputePrice_Spot_CheapestAZ | Go's GetComputePrice has no spot pricing path. PricingTermSpot is defined in models but GetComputePrice only handles OnDemand and reserved terms. Spot pricing is available only via GetSpotHistory (requires live EC2 credentials); there is no GetComputePrice spot path in Go to test cheapest-AZ selection. The Python test exercises get_compute_price(term=PricingTerm.SPOT) which internally calls boto3 describe_spot_price_history — a deliberate Go omission, not a gap. |
| TestNATGatewayAliasResolvesToEC2 | Python-only function. The Python test validates _resolve_service_code('nat_gateway') == 'AmazonEC2', which is a Python module-level alias map. No equivalent resolver function exists in Go; NAT Gateway pricing is routed through GetNetworkPrice using AWSDataTransfer, not AmazonEC2. There is no service-code alias resolver analogous to _resolve_service_code in Go. |

### tools-lookup

| Test Name | Reason |
|---|---|
| TestGetPrice_EmptyPublicPrices_NoResultsHint | Python production get_price (lookup.py:113-114) calls result.summary() unconditionally with no describe_catalog hint on empty prices. The Python test is tautological: it builds the 'no_results' dict inline and asserts on its own construction, never exercising real tool code. get_service_price has no Go analog (it is a Python AWS provider method, not a tool). Neither Python nor Go emit a describe_catalog hint on empty public_prices. |
| TestGetPrice_ProviderUnavailable_StructuredError | Behavior exists in Go (HandleGetPrice returns errResult with structured JSON when provider is nil) and is already covered by the existing TestGetPrice_ProviderNotConfigured test, which exercises the same code path and verifies the error key is a non-empty string. Adding a new test would be a near-duplicate. Go code at lookup.go:466-469 returns errResult({"error": "Provider '...' not configured. Available: [...]"}) via jsonText which always produces JSON. |
| TestSearchPricing_EmptyResults_NoResultsHint | Python production search_pricing (lookup.py:364-374) always returns the same structured response with provider/query/region/count/results/tip fields regardless of result count — no 'no_results' sentinel or filters hint exists. The Python test constructs its own response dict and asserts on its own literals (tautological). Go already matches Python behavior and has two passing tests (TestSearchPricing_NoResultsHint, TestSearchPricing_NoResultsWithDomain) whose comments explicitly document this finding. |

### bom

| Test Name | Reason |
|---|---|
| TestEstimateBOM_EmptyItems | Both Go and Python deliberately return {error: "No valid line items."} for empty items. The backlog description (returns valid empty response, not error) contradicts both implementations. No gap exists. Go code path: processBOMItems returns empty slices for empty input; HandleEstimateBOM hits the len(lineItems)==0 branch and returns a top-level error map, consistent with Python bom.py line 139-140. |

## Failed Implementations

No test implementations failed during this workplan. All 58 original items resolved as IMPLEMENTED (52), INVALID (3), or SKIP (3). 50 additional tests were added post-parity (azure-cosmosdb, azure-monitor, azure-frontdoor, gcp-vertex-ai, gcp-compute-sud, tools-lookup expansions, provider-term-invariants) for a running total of 102 implemented, 6 invalid/skip.

## Remaining Backlog

This workplan covered 58 of the 141 missing tests identified at project inception. The following domains and items were not assigned to domain agents in this run and remain open.

### aws-pricing (remaining)

- Additional reserved instance term combinations (Partial Upfront, No Upfront, 3-year variants) not covered by TestExtractReservedPrice_AllUpfront_Normalised
- GetComputePrice with graviton/arm64 instance families
- GetStoragePrice for EBS volume types (gp2, gp3, io1, io2, st1, sc1)
- GetStoragePrice for S3 storage classes (standard-ia, glacier, deep-archive)
- GetNetworkPrice for inter-region data transfer between specific region pairs
- RDS pricing for multi-AZ deployments

### aws-bulk (remaining)

- Streaming large bulk JSON files (pagination/chunking behaviour)
- Concurrent getProductsBulk calls sharing the same cache entry
- HTTP timeout and retry behaviour on slow bulk endpoints

### aws-finops (remaining)

- GetDiscountSummary with non-empty Savings Plans (coverage rate math)
- GetDiscountSummary with non-empty Reserved Instances (ri_count > 0)
- GetEffectivePrice with a valid Cost Explorer response (blended rate extraction)
- Error handling when Cost Explorer returns throttling (HTTP 429)

### azure (remaining)

- GetStoragePrice for blob storage tiers (hot, cool, archive)
- GetNetworkPrice for VPN Gateway SKUs
- GetSQLPrice for Hyperscale and Business Critical tiers
- GetComputePrice for spot/low-priority pricing
- Pagination handling when Azure Retail API returns multiple pages
- GetFrontDoorPrice for Zone 3 and Zone 4 regions

### gcp-networking (remaining)

- GetEgressPrice for intra-region (same-zone vs cross-zone within region)
- CDN cache fill rate from SKU (parallel to egress rate test)
- NAT data processing rate from SKU
- Cloud Armor advanced tier pricing

### gcp-compute (remaining)

- GetComputePrice for GPU instances (A100, V100, T4 attached GPUs)
- GetComputePrice for preemptible/spot instances
- GetComputePrice for sole-tenant nodes
- GetStoragePrice for persistent disk (pd-ssd, pd-balanced, pd-extreme)
- GetStoragePrice for Filestore tiers

### gcp-gke (remaining)

- GetGKEPrice for standard node pools with specific machine types
- GetGKEPrice for autopilot with GPU requests
- GetGKEPrice cluster management fee (standard mode, not zonal)
- GetMemstorePrice for Redis Cluster tier

### gcp-database (remaining)

- GetCloudSQLPrice for MySQL engine normalization (mysql, mariadb aliases)
- GetCloudSQLPrice for SQL Server engine
- GetCloudSQLPrice for storage pricing component
- GetBigQueryPrice for slot reservation pricing
- GetBigQueryPrice for storage pricing (active vs long-term)
- Spanner pricing (compute capacity + storage)
- Firestore pricing (read/write/delete operations + storage)

### tools-lookup (remaining)

- TestSearchPricing with domain filter narrowing results
- TestGetPrice returning multiple NormalizedPrice results (multi-region response)
- TestDescribeCatalog for each provider returning expected service list
- TestGetPrice with region override (non-default region)

### provider-contract (remaining)

- ErrNotSupported surfacing correctly through tool layer for each provider
- Auth-gated fields populated only when credentials are present (per-provider)
- Attribute key consistency across providers for equivalent concepts (e.g., instance_type, os, region)

### models (remaining)

- PricingSpec round-trip for each concrete spec type (ComputePricingSpec, StoragePricingSpec, etc.)
- NormalizedPrice.MonthlyCost() with non-hourly unit (per-GB, per-request)
- PricingTerm string parsing (from JSON string back to typed constant)

### cache (remaining)

- CacheManager with persistence across process restart (if disk-backed cache is added)
- Cache key collision resistance (different providers with same payload hash)
- Cache eviction under memory pressure

### bom (remaining)

- TestEstimateBOM with mixed providers (AWS + GCP items in one BOM)
- TestEstimateBOM total_cost aggregation math with multiple line items
- TestEstimateBOM with currency conversion (if supported)

### egress (remaining)

- Azure egress tier math (tiered pricing for Zone 1 vs Zone 2)
- GCP egress for premium vs standard network tier
- Cross-provider egress comparison tool (if implemented)
