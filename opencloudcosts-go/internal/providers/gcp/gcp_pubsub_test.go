// Package gcp — unit tests for Cloud Pub/Sub pricing (issue #79).
//
// SKU descriptions used in these fixtures are the exact wording captured by
// the live-catalog research report for issue #79 (Cloud Billing Catalog API,
// serviceId A1E8-BE35-7EBC), not invented strings — this keeps the matcher
// tests honest against real catalog text rather than merely self-consistent
// with the matcher's own substring choices. The matcher remains a
// case-insensitive substring match (not exact-string-equality) as a hedge
// against minor GCP wording drift over time.
package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Pub/Sub SKU helpers
// --------------------------------------------------------------------------

// pubsubSKUFlat builds a single-tier global SKU: one tier at
// startUsageAmount=0 carrying the rate directly. Used for every flat-rate
// Pub/Sub throughput/storage SKU.
func pubsubSKUFlat(desc, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": "PubSub",
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type": "GLOBAL",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": units,
								"nanos": float64(nanos),
							},
						},
					},
				},
			},
		},
	}
}

// pubsubSKUTwoTier builds a two-tier global SKU: tier1 (free) at
// startUsageAmount=0, tier2 (paid) at startUsageAmount=threshold/1024 (GCP's
// real Message Delivery Basic SKU publishes startUsageAmount in TiB, the
// same unit as its rate — see file header/gcp_pubsub.go — so threshold is
// accepted here in GiB, the caller-friendly unit, and converted to TiB
// on the way in; $40.00/TiB with a 10 GiB free allowance is real captured
// data, not merely self-consistent fixture math: 10 GiB == 10/1024 TiB).
// Models the Message Delivery Basic SKU: $0.00 (0-threshold GiB/mo), then
// the paid rate.
func pubsubSKUTwoTier(desc string, threshold float64, tier2Units string, tier2Nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": "PubSub",
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type": "GLOBAL",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": "0",
								"nanos": float64(0),
							},
						},
						map[string]any{
							"startUsageAmount": threshold / pubsubGiBPerTiB,
							"unitPrice": map[string]any{
								"units": tier2Units,
								"nanos": float64(tier2Nanos),
							},
						},
					},
				},
			},
		},
	}
}

// pubsubSKUResponse wraps Pub/Sub SKUs for the httptest server.
func pubsubSKUResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// fakePubSubSKUs returns one fake SKU per rate bucket, at the live-verified
// published rates (issue #79): $40/TiB (basic paid, SMT UDF), $50/TiB
// (BigQuery/Cloud-Storage-export/Bigtable/Kinesis-import), $80/TiB
// (Cloud-Storage-import/Azure/AWS-MSK/Confluent import), $60/TiB (SMT AI
// inference), and $0.27/GiB-month flat for all four storage SKUs.
func fakePubSubSKUs() []map[string]any {
	return []map[string]any{
		// SKU IDs in comments are the real catalog IDs from the research report.
		pubsubSKUTwoTier("Message Delivery Basic", 10, "40", 0),                         // 027D-B6C7-CCA2
		pubsubSKUFlat("Message Delivery to BigQuery", "50", 0),                          // FCD2-1531-9A6F
		pubsubSKUFlat("Message Delivery to Google Cloud Storage", "50", 0),              // 2792-0D28-83D3
		pubsubSKUFlat("Message Delivery to Bigtable", "50", 0),                          // 708B-266E-ED37
		pubsubSKUFlat("Message Delivery From Kinesis Data Streams (import)", "50", 0),   // 5B2B-763A-DAC0
		pubsubSKUFlat("Message Delivery From Google Cloud Storage (import)", "80", 0),   // C117-FD3D-1297
		pubsubSKUFlat("Message Delivery From Azure Event Hubs (import)", "80", 0),       // 4A9D-D3E2-DF4D
		pubsubSKUFlat("Message Delivery From AWS MSK (import)", "80", 0),                // 5790-7260-F0C6
		pubsubSKUFlat("Message Delivery From Confluent Cloud (import)", "80", 0),        // B99E-7224-CFF4
		pubsubSKUFlat("Message Transform Data Processing (UDF SMT)", "40", 0),           // 40F6-ACB3-8C8D
		pubsubSKUFlat("Message Transform Data Enrichment (AI Inference SMT)", "60", 0),  // 7627-6462-1DF2
		pubsubSKUFlat("Topics message backlog", "0", 270_000_000),                       // F9EB-D3E1-ABDF
		pubsubSKUFlat("Subscriptions message backlog", "0", 270_000_000),                // 3EAB-48F3-A0D5
		pubsubSKUFlat("Subscriptions retained acknowledged messages", "0", 270_000_000), // 3C0B-A83B-E6EE
		pubsubSKUFlat("Snapshots message backlog", "0", 270_000_000),                    // EAF4-71D0-17E0
	}
}

// --------------------------------------------------------------------------
// Rate-parsing / fetch tests
// --------------------------------------------------------------------------

// TestFetchPubSubRates_ParsesAllBuckets verifies fetchPubSubRates extracts
// every one of the 12 rate buckets, converting TiB rates to GiB.
func TestFetchPubSubRates_ParsesAllBuckets(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"BasicPaid", rates.BasicPaid, 40.0 / pubsubGiBPerTiB},
		{"BigQuery", rates.BigQuery, 50.0 / pubsubGiBPerTiB},
		{"CloudStorageExport", rates.CloudStorageExport, 50.0 / pubsubGiBPerTiB},
		{"Bigtable", rates.Bigtable, 50.0 / pubsubGiBPerTiB},
		{"KinesisImport", rates.KinesisImport, 50.0 / pubsubGiBPerTiB},
		{"CloudStorageImport", rates.CloudStorageImport, 80.0 / pubsubGiBPerTiB},
		{"AzureEventHubsImport", rates.AzureEventHubsImport, 80.0 / pubsubGiBPerTiB},
		{"AWSMSKImport", rates.AWSMSKImport, 80.0 / pubsubGiBPerTiB},
		{"ConfluentCloudImport", rates.ConfluentCloudImport, 80.0 / pubsubGiBPerTiB},
		{"SMTUDF", rates.SMTUDF, 40.0 / pubsubGiBPerTiB},
		{"SMTAIInference", rates.SMTAIInference, 60.0 / pubsubGiBPerTiB},
		{"Storage", rates.Storage, 0.27},
		{"BasicFreeTierGB", rates.BasicFreeTierGB, 10.0},
	}
	for _, c := range cases {
		if abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %.9f, want %.9f", c.name, c.got, c.want)
		}
	}
}

// TestFetchPubSubRates_FreeTierBoundaryReadFromLiveSKU verifies that
// BasicFreeTierGB is derived from the live Basic SKU's second-tier
// startUsageAmount rather than hardcoded: a fixture whose free-tier boundary
// is 20 GiB (not the pubsubBasicFreeTierGB constant's 10 GiB) must yield
// BasicFreeTierGB==20, proving the value is actually read off the SKU and
// the derivation isn't merely echoing the constant back.
func TestFetchPubSubRates_FreeTierBoundaryReadFromLiveSKU(t *testing.T) {
	skus := []map[string]any{
		pubsubSKUTwoTier("Message Delivery Basic", 20, "40", 0), // 20 GiB free tier, not the 10 GiB constant
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	if abs(rates.BasicFreeTierGB-20.0) > 1e-9 {
		t.Errorf("BasicFreeTierGB = %.6f, want 20.000000 (live SKU boundary, not the hardcoded 10 GiB constant)", rates.BasicFreeTierGB)
	}
	if rates.BasicFreeTierGB == pubsubBasicFreeTierGB {
		t.Errorf("BasicFreeTierGB = %.6f coincidentally equals the hardcoded constant %.6f; fixture must use a differing boundary to prove live derivation", rates.BasicFreeTierGB, pubsubBasicFreeTierGB)
	}
}

// TestFetchPubSubRates_FreeTierBoundaryFallsBackWithoutSecondTier verifies
// that a Basic SKU with fewer than two tiers leaves BasicFreeTierGB at its
// zero value (so pricePubSub's pickRate falls back to
// pubsubFallbackRates.BasicFreeTierGB) instead of panicking or guessing.
func TestFetchPubSubRates_FreeTierBoundaryFallsBackWithoutSecondTier(t *testing.T) {
	skus := []map[string]any{
		pubsubSKUFlat("Message Delivery Basic", "40", 0), // single-tier: no free-tier boundary to derive
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	if rates.BasicFreeTierGB != 0 {
		t.Errorf("BasicFreeTierGB = %.6f, want 0 (no second tier to derive a boundary from)", rates.BasicFreeTierGB)
	}
}

// TestFetchPubSubRates_Cached verifies that fetchPubSubRates reads its
// derived rate map from cache instead of calling fetchSKUs/HTTP at all.
func TestFetchPubSubRates_Cached(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchPubSubRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := pubsubRates{
		BasicPaid: 0.999, BigQuery: 0.888, CloudStorageExport: 0.777, Bigtable: 0.666,
		KinesisImport: 0.555, CloudStorageImport: 0.444, AzureEventHubsImport: 0.333,
		AWSMSKImport: 0.222, ConfluentCloudImport: 0.111, SMTUDF: 0.099, SMTAIInference: 0.088,
		Storage: 0.9,
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(pubsubRatesCacheKey, raw, p.cfg.MetadataTTL())

	rates := p.fetchPubSubRates(ctx)
	if rates != seeded {
		t.Errorf("fetchPubSubRates = %+v, want cached %+v", rates, seeded)
	}
}

// TestFetchPubSubRates_DuplicateMatchKeepsFirst verifies that if more than
// one SKU matches a non-storage bucket's substring pattern, the first match
// wins rather than a later SKU silently overwriting it.
func TestFetchPubSubRates_DuplicateMatchKeepsFirst(t *testing.T) {
	skus := []map[string]any{
		pubsubSKUFlat("Message Delivery to BigQuery", "50", 0), // first — should win
		pubsubSKUFlat("Message Delivery to BigQuery (duplicate catalog entry)", "999", 0),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	want := 50.0 / pubsubGiBPerTiB
	if abs(rates.BigQuery-want) > 1e-9 {
		t.Errorf("BigQuery = %.9f, want first match's %.9f (duplicate must not overwrite)", rates.BigQuery, want)
	}
}

// TestFetchPubSubRates_StorageAggregatesFourSKUsSilently verifies that all
// four storage SKUs (topic/subscription/retained-acked/snapshot backlog)
// collapse into the single Storage bucket WITHOUT firing the duplicate-match
// warning path used by every other bucket — this 4-to-1 aggregation is
// by-design (they are verified to share one identical rate), not an
// anomaly, so it must not be treated like an accidental duplicate SKU match.
func TestFetchPubSubRates_StorageAggregatesFourSKUsSilently(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	if abs(rates.Storage-0.27) > 1e-9 {
		t.Errorf("Storage = %.6f, want 0.270000 (all four storage SKUs aggregate to one rate)", rates.Storage)
	}
}

// TestFetchPubSubRates_StorageDivergenceKeepsFirst verifies that if a later
// storage SKU reports a rate that diverges from the first one matched, the
// first rate wins (warn-only, not an error and not an overwrite) rather than
// silently adopting whichever SKU happened to be paginated last.
func TestFetchPubSubRates_StorageDivergenceKeepsFirst(t *testing.T) {
	skus := []map[string]any{
		pubsubSKUFlat("Topics message backlog", "0", 270_000_000),        // first — $0.27/GiB-mo, should win
		pubsubSKUFlat("Subscriptions message backlog", "0", 990_000_000), // diverges — $0.99/GiB-mo
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	if abs(rates.Storage-0.27) > 1e-9 {
		t.Errorf("Storage = %.6f, want first match's 0.270000 (divergent later SKU must not overwrite)", rates.Storage)
	}
}

// TestFetchPubSubRates_SkipsNonGlobalMatch verifies that a SKU whose
// geoTaxonomy.type is present and not "GLOBAL" is skipped, even if its
// description would otherwise match.
func TestFetchPubSubRates_SkipsNonGlobalMatch(t *testing.T) {
	regional := pubsubSKUFlat("Message Delivery to BigQuery", "999", 0)
	regional["geoTaxonomy"] = map[string]any{"type": "REGIONAL"}

	skus := []map[string]any{
		regional,
		pubsubSKUFlat("Message Delivery to BigQuery", "50", 0), // the real, GLOBAL-scoped SKU
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchPubSubRates(ctx)
	want := 50.0 / pubsubGiBPerTiB
	if abs(rates.BigQuery-want) > 1e-9 {
		t.Errorf("BigQuery = %.9f, want %.9f (non-GLOBAL SKU must be skipped)", rates.BigQuery, want)
	}
}

// --------------------------------------------------------------------------
// pricePubSub — headline rate, scope, and dispatch tests
// --------------------------------------------------------------------------

// TestPricePubSub_ReturnsBothLineItems verifies pricePubSub returns exactly
// two NormalizedPrice entries (throughput + storage), both tagged
// region-invariant.
func TestPricePubSub_ReturnsBothLineItems(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{Destination: "basic"}
	prices, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected exactly two prices (throughput + storage), got %d", len(prices))
	}

	var throughput, storage *models.NormalizedPrice
	for i := range prices {
		switch prices[i].Unit {
		case models.PriceUnitPerGB:
			throughput = &prices[i]
		case models.PriceUnitPerGBMonth:
			storage = &prices[i]
		}
	}
	if throughput == nil {
		t.Fatal("no per_gb (throughput) price returned")
	}
	if storage == nil {
		t.Fatal("no per_gb_month (storage) price returned")
	}
	if abs(throughput.PricePerUnit-40.0/pubsubGiBPerTiB) > 1e-9 {
		t.Errorf("throughput rate = %.9f, want %.9f", throughput.PricePerUnit, 40.0/pubsubGiBPerTiB)
	}
	if abs(storage.PricePerUnit-0.27) > 1e-9 {
		t.Errorf("storage rate = %.6f, want 0.270000", storage.PricePerUnit)
	}
	for _, pr := range []*models.NormalizedPrice{throughput, storage} {
		if pr.Region != "global" || pr.Attributes["scope"] != "global" {
			t.Errorf("region/scope not tagged global: region=%q attrs=%v", pr.Region, pr.Attributes)
		}
	}
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when all Pub/Sub SKUs are present")
	}
}

// TestPricePubSub_AllDestinations verifies every valid destination selects
// its own distinct throughput rate.
func TestPricePubSub_AllDestinations(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	cases := []struct {
		destination string
		want        float64
	}{
		{"basic", 40.0 / pubsubGiBPerTiB},
		{"bigquery", 50.0 / pubsubGiBPerTiB},
		{"cloud_storage_export", 50.0 / pubsubGiBPerTiB},
		{"bigtable", 50.0 / pubsubGiBPerTiB},
		{"kinesis_import", 50.0 / pubsubGiBPerTiB},
		{"cloud_storage_import", 80.0 / pubsubGiBPerTiB},
		{"azure_event_hubs_import", 80.0 / pubsubGiBPerTiB},
		{"aws_msk_import", 80.0 / pubsubGiBPerTiB},
		{"confluent_cloud_import", 80.0 / pubsubGiBPerTiB},
		{"smt_udf", 40.0 / pubsubGiBPerTiB},
		{"smt_ai_inference", 60.0 / pubsubGiBPerTiB},
	}
	for _, c := range cases {
		t.Run(c.destination, func(t *testing.T) {
			spec := &models.PubSubPricingSpec{Destination: c.destination}
			prices, _, err := p.pricePubSub(ctx, spec)
			if err != nil {
				t.Fatalf("pricePubSub: %v", err)
			}
			for _, pr := range prices {
				if pr.Unit == models.PriceUnitPerGB && abs(pr.PricePerUnit-c.want) > 1e-9 {
					t.Errorf("destination=%s: throughput rate = %.9f, want %.9f", c.destination, pr.PricePerUnit, c.want)
				}
			}
		})
	}
}

// TestPricePubSub_DefaultDestinationIsBasic verifies an empty Destination
// defaults to "basic".
func TestPricePubSub_DefaultDestinationIsBasic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{}
	prices, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	if breakdown["destination"] != "basic" {
		t.Errorf("breakdown[destination] = %v, want %q", breakdown["destination"], "basic")
	}
	for _, pr := range prices {
		if pr.Unit == models.PriceUnitPerGB && abs(pr.PricePerUnit-40.0/pubsubGiBPerTiB) > 1e-9 {
			t.Errorf("default destination throughput rate = %.9f, want basic paid rate", pr.PricePerUnit)
		}
	}
}

// TestPricePubSub_InvalidDestinationErrors verifies an unrecognized
// destination is rejected — destination DOES select the rate, unlike
// storage_type.
func TestPricePubSub_InvalidDestinationErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{Destination: "not_a_real_destination"}
	_, _, err := p.pricePubSub(ctx, spec)
	if err == nil {
		t.Fatal("expected an error for an invalid destination, got nil")
	}
}

// TestPricePubSub_StorageTypeIsPriceNeutral verifies that every valid
// storage_type resolves to the identical storage rate — storage_type is
// informational only and must never select a different rate.
func TestPricePubSub_StorageTypeIsPriceNeutral(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	for _, st := range []string{"topic_backlog", "subscription_backlog", "retained_acked_messages", "snapshot_backlog"} {
		t.Run(st, func(t *testing.T) {
			spec := &models.PubSubPricingSpec{StorageType: st}
			prices, _, err := p.pricePubSub(ctx, spec)
			if err != nil {
				t.Fatalf("pricePubSub: %v", err)
			}
			for _, pr := range prices {
				if pr.Unit == models.PriceUnitPerGBMonth && abs(pr.PricePerUnit-0.27) > 1e-9 {
					t.Errorf("storage_type=%s: storage rate = %.6f, want 0.270000 (price-neutral)", st, pr.PricePerUnit)
				}
			}
		})
	}
}

// TestPricePubSub_UnrecognizedStorageTypeIsInformationalOnly verifies an
// unrecognized storage_type never errors and still prices at the shared
// storage rate — mirroring DNS's zone_type precedent.
func TestPricePubSub_UnrecognizedStorageTypeIsInformationalOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{StorageType: "some_future_storage_type"}
	prices, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: unexpected error for unrecognized storage_type: %v", err)
	}
	if u, ok := breakdown["storage_type_unrecognized"]; !ok || u != true {
		t.Errorf("expected breakdown[storage_type_unrecognized]=true, got %v (ok=%v)", u, ok)
	}
	for _, pr := range prices {
		if pr.Unit == models.PriceUnitPerGBMonth && abs(pr.PricePerUnit-0.27) > 1e-9 {
			t.Errorf("unrecognized storage_type: storage rate = %.6f, want 0.270000", pr.PricePerUnit)
		}
	}
}

// TestPricePubSub_DispatchViaGetPrice verifies GetPrice routes a
// PubSubPricingSpec through Supports/getPart3Price to pricePubSub.
func TestPricePubSub_DispatchViaGetPrice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	if !p.Supports(models.PricingDomainMessaging, "pubsub") {
		t.Fatal("Supports(messaging, pubsub) = false, want true")
	}

	spec := &models.PubSubPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Domain:  models.PricingDomainMessaging,
			Service: "pubsub",
		},
		Destination: "basic",
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if len(result.PublicPrices) != 2 {
		t.Fatalf("GetPrice did not dispatch to pricePubSub: %+v", result.PublicPrices)
	}
	for _, pr := range result.PublicPrices {
		if pr.Service != "pubsub" {
			t.Errorf("Service = %q, want %q", pr.Service, "pubsub")
		}
	}
}

// --------------------------------------------------------------------------
// Tiered cost-math tests — Basic free tier
// --------------------------------------------------------------------------

// TestPricePubSub_BasicCostMath_BelowFreeTier verifies throughput_monthly_cost
// is $0.00 for Basic usage strictly within the 10 GiB/month free allowance.
func TestPricePubSub_BasicCostMath_BelowFreeTier(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 5.0
	spec := &models.PubSubPricingSpec{Destination: "basic", ThroughputGBPerMonth: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["throughput_monthly_cost"], "throughput_monthly_cost")
	if abs(got-0) > 1e-9 {
		t.Errorf("throughput_monthly_cost = %.6f, want 0.000000 (within free tier)", got)
	}
}

// TestPricePubSub_BasicCostMath_AtFreeTierThreshold verifies exactly 10 GiB
// (the free-tier boundary) costs $0.00 — none spills into the paid tier.
func TestPricePubSub_BasicCostMath_AtFreeTierThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 10.0
	spec := &models.PubSubPricingSpec{Destination: "basic", ThroughputGBPerMonth: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["throughput_monthly_cost"], "throughput_monthly_cost")
	if abs(got-0) > 1e-9 {
		t.Errorf("throughput_monthly_cost at threshold = %.6f, want 0.000000 (all free)", got)
	}
}

// TestPricePubSub_BasicCostMath_CrossesFreeTier verifies cost splits
// correctly when usage crosses the 10 GiB/month free allowance: 15 GiB =
// 10 GiB free + 5 GiB @ paid rate, NOT 15 GiB @ paid rate.
func TestPricePubSub_BasicCostMath_CrossesFreeTier(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 15.0
	spec := &models.PubSubPricingSpec{Destination: "basic", ThroughputGBPerMonth: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["throughput_monthly_cost"], "throughput_monthly_cost")
	want := 5.0 * (40.0 / pubsubGiBPerTiB)
	if abs(got-want) > 1e-6 {
		t.Errorf("throughput_monthly_cost = %.6f, want %.6f (tiered split, not flat paidRate*qty)", got, want)
	}
}

// TestPricePubSub_BasicCostMath_UsesLiveFreeTierBoundary verifies that
// pricePubSub's tiered throughput math actually uses the live-derived free
// allowance (20 GiB here, not the pubsubBasicFreeTierGB==10 hardcoded
// constant): 15 GiB of usage must be entirely within the (live) 20 GiB free
// allowance, producing $0.00 throughput cost, not a >0 cost split against
// the constant's 10 GiB boundary.
func TestPricePubSub_BasicCostMath_UsesLiveFreeTierBoundary(t *testing.T) {
	skus := []map[string]any{
		pubsubSKUTwoTier("Message Delivery Basic", 20, "40", 0), // live free tier: 20 GiB
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 15.0
	spec := &models.PubSubPricingSpec{Destination: "basic", ThroughputGBPerMonth: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["throughput_monthly_cost"], "throughput_monthly_cost")
	if abs(got-0) > 1e-9 {
		t.Errorf("throughput_monthly_cost = %.6f, want 0 (15 GiB is within the live-derived 20 GiB free allowance, not the hardcoded 10 GiB constant)", got)
	}
	if freeTier := breakdown["free_tier_gb_per_month"]; freeTier != 20.0 {
		t.Errorf("free_tier_gb_per_month = %v, want 20 (live-derived)", freeTier)
	}
}

// TestPricePubSub_NonBasicCostMath_IsFlat verifies non-Basic destinations
// have no free tier: cost = rate * qty at every volume.
func TestPricePubSub_NonBasicCostMath_IsFlat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 2.0
	spec := &models.PubSubPricingSpec{Destination: "bigquery", ThroughputGBPerMonth: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["throughput_monthly_cost"], "throughput_monthly_cost")
	want := qty * (50.0 / pubsubGiBPerTiB)
	if abs(got-want) > 1e-9 {
		t.Errorf("throughput_monthly_cost = %.9f, want %.9f (flat, no free tier)", got, want)
	}
}

// TestPricePubSub_StorageCostMath verifies storage_monthly_cost is a flat
// rate*qty product (storage has no tiering or free allowance).
func TestPricePubSub_StorageCostMath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 100.0
	spec := &models.PubSubPricingSpec{StorageGB: &qty}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	got := mustFloat64(t, breakdown["storage_monthly_cost"], "storage_monthly_cost")
	want := 100.0 * 0.27
	if abs(got-want) > 1e-6 {
		t.Errorf("storage_monthly_cost = %.4f, want %.4f", got, want)
	}
}

// TestPricePubSub_MonthlyCost_SumsBothDimensions verifies the top-level
// monthly_cost is the sum of throughput and storage when both quantities
// are supplied.
func TestPricePubSub_MonthlyCost_SumsBothDimensions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	throughputQty := 15.0
	storageQty := 100.0
	spec := &models.PubSubPricingSpec{
		Destination:          "basic",
		ThroughputGBPerMonth: &throughputQty,
		StorageGB:            &storageQty,
	}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	throughputCost := 5.0 * (40.0 / pubsubGiBPerTiB)
	storageCost := 100.0 * 0.27
	got := mustFloat64(t, breakdown["monthly_cost"], "monthly_cost")
	want := throughputCost + storageCost
	if abs(got-want) > 1e-6 {
		t.Errorf("monthly_cost = %.6f, want %.6f (sum of both dimensions)", got, want)
	}
}

// TestPricePubSub_NoMonthlyCostWithoutQuantities verifies no monthly_cost
// key is present when neither ThroughputGBPerMonth nor StorageGB is set.
func TestPricePubSub_NoMonthlyCostWithoutQuantities(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(fakePubSubSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{}
	_, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	if _, ok := breakdown["monthly_cost"]; ok {
		t.Error("expected no monthly_cost key when no quantities are supplied")
	}
}

// --------------------------------------------------------------------------
// Fallback tests
// --------------------------------------------------------------------------

// TestPricePubSub_Fallback verifies pricePubSub falls back to the published
// static rates when the live SKU catalog returns nothing.
func TestPricePubSub_Fallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pubsubSKUResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.PubSubPricingSpec{Destination: "basic"}
	prices, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub (fallback): %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		switch pr.Unit {
		case models.PriceUnitPerGB:
			if abs(pr.PricePerUnit-pubsubFallbackRates.BasicPaid) > 1e-9 {
				t.Errorf("fallback throughput rate = %.9f, want %.9f", pr.PricePerUnit, pubsubFallbackRates.BasicPaid)
			}
		case models.PriceUnitPerGBMonth:
			if abs(pr.PricePerUnit-pubsubFallbackRates.Storage) > 1e-9 {
				t.Errorf("fallback storage rate = %.6f, want %.6f", pr.PricePerUnit, pubsubFallbackRates.Storage)
			}
		}
	}
}

// TestPricePubSub_PartialFallback_ThroughputOnly verifies that if only the
// storage rate resolves live (throughput rate missing/zero), only the
// throughput side falls back — the two dimensions are priced by
// independent pickRate calls, unlike DNS/KMS's tiered dimensions which
// share an all-or-nothing guard across tiers of the SAME dimension.
func TestPricePubSub_PartialFallback_ThroughputOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchPubSubRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := pubsubRates{Storage: 0.27} // throughput rates all zero/missing
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(pubsubRatesCacheKey, raw, p.cfg.MetadataTTL())

	spec := &models.PubSubPricingSpec{Destination: "bigquery"}
	prices, breakdown, err := p.pricePubSub(ctx, spec)
	if err != nil {
		t.Fatalf("pricePubSub: %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true when throughput rate is missing, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		switch pr.Unit {
		case models.PriceUnitPerGB:
			if abs(pr.PricePerUnit-pubsubFallbackRates.BigQuery) > 1e-9 {
				t.Errorf("throughput rate = %.9f, want fallback %.9f", pr.PricePerUnit, pubsubFallbackRates.BigQuery)
			}
		case models.PriceUnitPerGBMonth:
			if abs(pr.PricePerUnit-0.27) > 1e-9 {
				t.Errorf("storage rate = %.6f, want live 0.270000 (should NOT have fallen back)", pr.PricePerUnit)
			}
		}
	}
}
