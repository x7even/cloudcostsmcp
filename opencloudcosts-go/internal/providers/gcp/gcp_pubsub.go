// gcp_pubsub.go — GCP Cloud Pub/Sub pricing (domain=messaging, service=pubsub).
//
// Cloud Pub/Sub bills along two independent, region-invariant dimensions:
//   - a per-GiB message-throughput charge, bifurcated by destination:
//   - "basic" message delivery includes a 10 GiB/month free allowance,
//     then $40.00/TiB ($0.0390625/GiB) thereafter;
//   - BigQuery / Cloud Storage (export) / Bigtable direct-write and
//     Kinesis-import destinations are each a flat $50.00/TiB
//     ($0.048828125/GiB), no free tier;
//   - Cloud Storage (import) / Azure Event Hubs (import) / AWS MSK
//     (import) / Confluent Cloud (import) are each a flat $80.00/TiB
//     ($0.078125/GiB), no free tier;
//   - Single Message Transform (SMT) UDF throughput is a flat
//     $40.00/TiB ($0.0390625/GiB); SMT AI-inference throughput is a
//     flat $60.00/TiB ($0.05859375/GiB); and
//   - a per-GiB-month storage charge ($0.27/GiB-month) for retained message
//     backlog (topics, subscriptions, snapshots) and retained acknowledged
//     messages — all four share one identical rate.
//
// All rates below were verified live against the GCP Cloud Billing Catalog
// API (service ID A1E8-BE35-7EBC, "Cloud Pub/Sub", 77 SKUs total, fully
// paginated — see issue #79) and cross-checked against
// https://cloud.google.com/pubsub/pricing. Every one of the SKUs in scope
// here has serviceRegions==["global"] and geoTaxonomy.type either "GLOBAL"
// or absent (never "REGIONAL") — price does not vary by region, confirmed
// (not merely assumed, per the issue's "candidate for scope:global"
// framing) by pricing-page prose ("in all Google Cloud regions"). This file
// never queries or matches on region; every returned NormalizedPrice is
// tagged Region="global" and Attributes["scope"]="global", mirroring the
// Cloud DNS (#78) / Cloud KMS (#77) / External IP Charge (#76) precedent.
//
// GCP publishes these rates per TiB; this file converts every throughput
// rate to per-GiB (dividing by pubsubGiBPerTiB=1024) at fetch time so it can
// reuse models.PriceUnitPerGB — the existing PriceUnit enum has no TiB unit,
// and gcp_networking.go already treats "GB" as GiB by convention (its
// internet-egress tier thresholds are 1024/10240 GiB). Storage rates are
// already published per GiB-month and need no conversion.
//
// Explicitly out of scope (deliberately, not gaps):
//   - Pub/Sub Lite (a distinct service, serviceId 3A1B-66C4-2BAE) — a
//     different pricing model (provisioned capacity + storage), not
//     covered by this spec.
//   - The 61 Network/egress SKUs also present under this same service ID.
//     They are tagged serviceRegions==["global"] at the API level too, but
//     encode actual geography as continent-pair corridors inside the SKU
//     description string (e.g. "... from Americas to EMEA") rather than a
//     flat global rate — a materially more complex shape deferred to a
//     future, dedicated egress spec. One of these SKUs ("Pub/Sub Google
//     Egress") shows a $0.00 live rate while the public pricing page prose
//     states egress is "not exempt" from standard network pricing — an
//     unresolved discrepancy flagged here for whoever implements Pub/Sub
//     egress next; it does not affect this spec since egress is out of
//     scope.
//   - Schema Registry, seek/replay, filtered messages, and dead-letter
//     topics/ordering keys are confirmed NOT billed as separate SKUs (no
//     corresponding SKU exists in the full 77-SKU inventory) — these are
//     genuinely free, not gaps in this implementation.
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// pubsubServiceID is the GCP Cloud Billing Catalog service ID for
// "Cloud Pub/Sub" — verified live 2026-07-06.
const pubsubServiceID = "A1E8-BE35-7EBC"

// pubsubSourceURL is the canonical public pricing page used for
// cross-checking and as the SourceURL stamped on returned prices.
const pubsubSourceURL = "https://cloud.google.com/pubsub/pricing"

// pubsubGiBPerTiB converts GCP's published $/TiB throughput rates to $/GiB
// (models.PriceUnitPerGB) — see file header for rationale.
const pubsubGiBPerTiB = 1024.0

// pubsubBasicFreeTierGB is the monthly free allowance (in GiB) for Basic
// message delivery before the paid throughput rate applies (verified live:
// tiered SKU has a $0.00 tier at startUsageAmount=0, then the paid rate
// starting at 10 GiB).
const pubsubBasicFreeTierGB = 10.0

// pubsubRates holds the derived, region-invariant Cloud Pub/Sub rates, all
// expressed per-GiB (throughput) or per-GiB-month (storage), cached under
// pubsubRatesCacheKey so repeated calls for different (destination,
// storage_type, quantity) combinations don't re-fetch/re-scan the full
// Pub/Sub SKU catalog.
type pubsubRates struct {
	// Throughput rates, $/GiB.
	BasicPaid            float64 `json:"basic_paid"`              // paid tier, after 10 GiB/mo free
	BigQuery             float64 `json:"bigquery"`                // BigQuery direct-write ingestion
	CloudStorageExport   float64 `json:"cloud_storage_export"`    // Cloud Storage direct-write (export)
	Bigtable             float64 `json:"bigtable"`                // Bigtable direct-write ingestion
	KinesisImport        float64 `json:"kinesis_import"`          // Kinesis cross-cloud import
	CloudStorageImport   float64 `json:"cloud_storage_import"`    // Cloud Storage import
	AzureEventHubsImport float64 `json:"azure_event_hubs_import"` // Azure Event Hubs import
	AWSMSKImport         float64 `json:"aws_msk_import"`          // AWS MSK import
	ConfluentCloudImport float64 `json:"confluent_cloud_import"`  // Confluent Cloud import
	SMTUDF               float64 `json:"smt_udf"`                 // Single Message Transform: UDF
	SMTAIInference       float64 `json:"smt_ai_inference"`        // Single Message Transform: AI inference

	// Storage rate, $/GiB-month — shared by topic backlog, subscription
	// backlog, retained acknowledged messages, and snapshot backlog.
	Storage float64 `json:"storage"`
}

// pubsubFallbackRates holds the published, live-verified rates (issue #79)
// used when the live SKU catalog is unavailable or a description match
// fails. Computed from the published $/TiB rates divided by
// pubsubGiBPerTiB, except Storage which is already $/GiB-month.
var pubsubFallbackRates = pubsubRates{
	BasicPaid:            40.00 / pubsubGiBPerTiB,
	BigQuery:             50.00 / pubsubGiBPerTiB,
	CloudStorageExport:   50.00 / pubsubGiBPerTiB,
	Bigtable:             50.00 / pubsubGiBPerTiB,
	KinesisImport:        50.00 / pubsubGiBPerTiB,
	CloudStorageImport:   80.00 / pubsubGiBPerTiB,
	AzureEventHubsImport: 80.00 / pubsubGiBPerTiB,
	AWSMSKImport:         80.00 / pubsubGiBPerTiB,
	ConfluentCloudImport: 80.00 / pubsubGiBPerTiB,
	SMTUDF:               40.00 / pubsubGiBPerTiB,
	SMTAIInference:       60.00 / pubsubGiBPerTiB,
	Storage:              0.27,
}

// pubsubRatesCacheKey caches the derived Cloud Pub/Sub rates.
const pubsubRatesCacheKey = "gcp:pubsub:rates"

// pubsubValidDestinations is the complete set of recognized
// PubSubPricingSpec.Destination values. Destination DOES select the
// throughput rate, so pricePubSub rejects anything outside this set with an
// explicit error rather than silently falling through into a
// wrong-but-plausible bucket.
var pubsubValidDestinations = map[string]bool{
	"basic":                   true,
	"bigquery":                true,
	"cloud_storage_export":    true,
	"bigtable":                true,
	"kinesis_import":          true,
	"cloud_storage_import":    true,
	"azure_event_hubs_import": true,
	"aws_msk_import":          true,
	"confluent_cloud_import":  true,
	"smt_udf":                 true,
	"smt_ai_inference":        true,
}

// pubsubKnownStorageTypes is the set of PubSubPricingSpec.StorageType values
// verified against the public pricing page. storage_type is a purely
// descriptive, non-rate-selecting attribute (see file header) — all four
// share the identical storage rate — so an unrecognized value is surfaced
// as an informational breakdown note rather than rejected, mirroring
// DNSPricingSpec.ZoneType's role in gcp_dns.go.
var pubsubKnownStorageTypes = map[string]bool{
	"topic_backlog":           true,
	"subscription_backlog":    true,
	"retained_acked_messages": true,
	"snapshot_backlog":        true,
}

// fetchPubSubRates returns the live, derived Cloud Pub/Sub rates, caching
// the result. A zero field means no matching SKU was found for that rate
// bucket and the caller should fall back to pubsubFallbackRates.
//
// Matching is case-insensitive substring matching against SKU descriptions
// (never exact-string-equality — the same convention as gcp_dns.go/
// gcp_kms.go, since GCP catalog wording can drift), reusing skuPaidPrice
// (gcp_ai.go — first tier with startUsageAmount>0) for the Basic SKU's paid
// tier and skuPrice (gcp.go — first tier, startUsageAmount==0) for every
// other flat-rate SKU. A defensive GLOBAL geoTaxonomy filter (mirroring
// gcp_dns.go's refinement) skips any SKU whose geoTaxonomy.type is present
// and non-GLOBAL, so a regional SKU that happens to share substring wording
// is never mistaken for a Pub/Sub rate.
//
// The four storage SKUs (topic/subscription/retained-acked/snapshot
// backlog) are a deliberate many-to-one exception to the usual
// one-bucket-per-SKU duplicate-match guard: they are verified to share one
// identical rate, so all four are expected to match the single Storage
// bucket. A slog.Warn fires only if a later storage SKU's rate actually
// differs from the first one matched — the real drift signal — rather than
// on every routine multi-SKU aggregation.
func (p *Provider) fetchPubSubRates(ctx context.Context) pubsubRates {
	if raw, ok := p.cache.GetMetadata(pubsubRatesCacheKey); ok {
		var r pubsubRates
		if err := json.Unmarshal(raw, &r); err == nil {
			return r
		}
	}

	var rates pubsubRates
	skus, err := p.fetchSKUs(ctx, pubsubServiceID)
	if err != nil {
		slog.Warn("gcp pubsub: fetch SKUs failed", "err", err)
		return rates
	}

	matched := map[string]bool{}
	storageMatchedDesc := ""

	setOnce := func(bucket string, dest *float64, desc string, giBRate float64) {
		if matched[bucket] {
			slog.Warn("gcp pubsub: multiple SKUs matched bucket; keeping first match", "bucket", bucket, "description", desc)
			return
		}
		matched[bucket] = true
		*dest = giBRate
	}

	for _, sku := range skus {
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)

		if geo, ok := sku["geoTaxonomy"].(map[string]any); ok {
			if geoType, _ := geo["type"].(string); geoType != "" && geoType != "GLOBAL" {
				continue
			}
		}

		switch {
		case strings.Contains(descLower, "message delivery basic"):
			setOnce("basic", &rates.BasicPaid, desc, skuPaidPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "to bigquery"):
			setOnce("bigquery", &rates.BigQuery, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "to google cloud storage"):
			setOnce("cloud_storage_export", &rates.CloudStorageExport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "to bigtable"):
			setOnce("bigtable", &rates.Bigtable, desc, skuPrice(sku)/pubsubGiBPerTiB)

		// Kinesis, Azure Event Hubs, AWS MSK, and Confluent Cloud only ever
		// appear as import sources (no "export to" counterpart exists for
		// them), so matching on the service name alone is sufficient and
		// more robust to description-wording drift than requiring an exact
		// "from ..." prefix. Google Cloud Storage is the one destination
		// that genuinely needs "to"/"from" disambiguation, since it has
		// both an export (direct-write) and an import SKU — see the two
		// cases above/below.
		case strings.Contains(descLower, "kinesis"):
			setOnce("kinesis_import", &rates.KinesisImport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "from google cloud storage"):
			setOnce("cloud_storage_import", &rates.CloudStorageImport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "azure event hub"):
			setOnce("azure_event_hubs_import", &rates.AzureEventHubsImport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "msk"):
			setOnce("aws_msk_import", &rates.AWSMSKImport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "confluent"):
			setOnce("confluent_cloud_import", &rates.ConfluentCloudImport, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "smt") && strings.Contains(descLower, "ai inference"):
			setOnce("smt_ai_inference", &rates.SMTAIInference, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "smt") || strings.Contains(descLower, "single message transform"):
			setOnce("smt_udf", &rates.SMTUDF, desc, skuPrice(sku)/pubsubGiBPerTiB)

		case strings.Contains(descLower, "message backlog") ||
			strings.Contains(descLower, "retained acknowledged messages") ||
			strings.Contains(descLower, "snapshot"):
			r := skuPrice(sku)
			if r <= 0 {
				continue
			}
			if rates.Storage == 0 {
				rates.Storage = r
				storageMatchedDesc = desc
			} else if r != rates.Storage {
				slog.Warn("gcp pubsub: storage SKU rate diverges from first match", "first_description", storageMatchedDesc, "first_rate", rates.Storage, "description", desc, "rate", r)
			}
		}
	}

	if raw, err := json.Marshal(rates); err == nil {
		p.cache.SetMetadata(pubsubRatesCacheKey, raw, p.cfg.MetadataTTL())
	}
	return rates
}

// newPubSubPrice builds a Cloud Pub/Sub NormalizedPrice with the fields
// common to every Pub/Sub line item (Provider/Service/ProductFamily/
// PricingTerm/Currency) already filled in and global scope already stamped,
// so pricePubSub's per-line-item construction only needs to supply what
// actually differs.
func newPubSubPrice(skuID, description string, pricePerUnit float64, unit models.PriceUnit, attrs map[string]string) *models.NormalizedPrice {
	price := &models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "pubsub",
		SKUID:         skuID,
		ProductFamily: "Cloud Pub/Sub",
		Description:   description,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerUnit,
		Unit:          unit,
		Currency:      "USD",
		Attributes:    attrs,
	}
	stampGlobalScope(price)
	return price
}

// pubsubDestinationDescriptions maps each valid Destination to its
// human-readable line-item description.
var pubsubDestinationDescriptions = map[string]string{
	"basic":                   "Message Delivery Basic (paid rate; first 10 GiB/month free)",
	"bigquery":                "Message Delivery to BigQuery (direct-write ingestion)",
	"cloud_storage_export":    "Message Delivery to Cloud Storage (direct-write export)",
	"bigtable":                "Message Delivery to Bigtable (direct-write ingestion)",
	"kinesis_import":          "Message Import from Amazon Kinesis",
	"cloud_storage_import":    "Message Import from Cloud Storage",
	"azure_event_hubs_import": "Message Import from Azure Event Hubs",
	"aws_msk_import":          "Message Import from AWS MSK",
	"confluent_cloud_import":  "Message Import from Confluent Cloud",
	"smt_udf":                 "Single Message Transform (UDF) throughput",
	"smt_ai_inference":        "Single Message Transform (AI inference) throughput",
}

// pricePubSub returns Cloud Pub/Sub pricing for the given PubSubPricingSpec:
// one NormalizedPrice for the message-throughput rate (selected by
// destination) and one for the message-storage rate (shared by every
// storage_type). Throughput and storage are independent, complementary
// billing dimensions (like Cloud DNS's zone/query pair in gcp_dns.go), so
// both line items are always returned together.
func (p *Provider) pricePubSub(
	ctx context.Context,
	spec *models.PubSubPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	destination := strings.ToLower(spec.Destination)
	if destination == "" {
		destination = "basic"
	}
	storageType := strings.ToLower(spec.StorageType)
	if storageType == "" {
		storageType = "topic_backlog"
	}

	// Destination DOES select the throughput rate — reject unrecognized
	// values explicitly rather than letting them fall through into a
	// wrong-but-plausible bucket. storage_type never changes price (see
	// file header / pubsubKnownStorageTypes doc) so it is validated
	// informationally only, below, and never errors.
	if !pubsubValidDestinations[destination] {
		return nil, nil, fmt.Errorf(
			"gcp pubsub: invalid destination %q: must be one of 'basic', 'bigquery', 'cloud_storage_export', "+
				"'bigtable', 'kinesis_import', 'cloud_storage_import', 'azure_event_hubs_import', "+
				"'aws_msk_import', 'confluent_cloud_import', 'smt_udf', 'smt_ai_inference'",
			spec.Destination,
		)
	}
	storageTypeRecognized := pubsubKnownStorageTypes[storageType]

	rates := p.fetchPubSubRates(ctx)

	var throughputRate float64
	var throughputFallback bool
	switch destination {
	case "basic":
		throughputRate, throughputFallback = pickRate(rates.BasicPaid, pubsubFallbackRates.BasicPaid)
	case "bigquery":
		throughputRate, throughputFallback = pickRate(rates.BigQuery, pubsubFallbackRates.BigQuery)
	case "cloud_storage_export":
		throughputRate, throughputFallback = pickRate(rates.CloudStorageExport, pubsubFallbackRates.CloudStorageExport)
	case "bigtable":
		throughputRate, throughputFallback = pickRate(rates.Bigtable, pubsubFallbackRates.Bigtable)
	case "kinesis_import":
		throughputRate, throughputFallback = pickRate(rates.KinesisImport, pubsubFallbackRates.KinesisImport)
	case "cloud_storage_import":
		throughputRate, throughputFallback = pickRate(rates.CloudStorageImport, pubsubFallbackRates.CloudStorageImport)
	case "azure_event_hubs_import":
		throughputRate, throughputFallback = pickRate(rates.AzureEventHubsImport, pubsubFallbackRates.AzureEventHubsImport)
	case "aws_msk_import":
		throughputRate, throughputFallback = pickRate(rates.AWSMSKImport, pubsubFallbackRates.AWSMSKImport)
	case "confluent_cloud_import":
		throughputRate, throughputFallback = pickRate(rates.ConfluentCloudImport, pubsubFallbackRates.ConfluentCloudImport)
	case "smt_udf":
		throughputRate, throughputFallback = pickRate(rates.SMTUDF, pubsubFallbackRates.SMTUDF)
	case "smt_ai_inference":
		throughputRate, throughputFallback = pickRate(rates.SMTAIInference, pubsubFallbackRates.SMTAIInference)
	}
	storageRate, storageFallback := pickRate(rates.Storage, pubsubFallbackRates.Storage)

	throughputAttrs := map[string]string{"destination": destination}
	storageAttrs := map[string]string{"storage_type": storageType}

	throughputPrice := newPubSubPrice(
		fmt.Sprintf("gcp:pubsub:throughput:%s", destination),
		pubsubDestinationDescriptions[destination],
		throughputRate,
		models.PriceUnitPerGB,
		throughputAttrs,
	)
	storagePrice := newPubSubPrice(
		"gcp:pubsub:storage",
		"Message Storage (topic/subscription/snapshot backlog and retained acknowledged messages share one rate)",
		storageRate,
		models.PriceUnitPerGBMonth,
		storageAttrs,
	)

	breakdown := map[string]any{
		"destination":       destination,
		"storage_type":      storageType,
		"throughput_rate":   breakdownMoney(throughputRate, "/GiB"),
		"storage_rate":      breakdownMoney(storageRate, "/GiB-month"),
		"storage_type_note": "storage_type does not affect price: topic backlog, subscription backlog, retained acknowledged messages, and snapshot backlog all share the same storage rate (verified live).",
	}
	if destination == "basic" {
		breakdown["free_tier_gb_per_month"] = pubsubBasicFreeTierGB
		breakdown["basic_note"] = "Headline throughput rate is the PAID rate that applies after the 10 GiB/month free allowance is exceeded; usage within the free allowance is $0.00."
	}
	if !storageTypeRecognized {
		breakdown["storage_type_unrecognized"] = true
		breakdown["storage_type_unrecognized_note"] = fmt.Sprintf(
			"storage_type %q is not one of the verified values (topic_backlog, subscription_backlog, retained_acked_messages, snapshot_backlog); this does not affect price, since storage_type never changes the rate.",
			spec.StorageType,
		)
	}
	if throughputFallback || storageFallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rate(s); live SKU catalog unavailable or returned no match. Verify current rates at " + pubsubSourceURL + "."
	}

	// throughput_monthly_cost and storage_monthly_cost must account for the
	// Basic free allowance — a flat rate*quantity product would overcharge
	// Basic destination usage at every volume, not just near a tier
	// boundary. Both dimensions are expressed as an []egressTier list and
	// priced through the shared addTieredEstimate helper (gcp_dns.go),
	// which itself calls computeTieredCost (gcp_networking.go), rather than
	// hand-rolling clamp-and-subtract arithmetic. egressTier.thresholdGB is
	// used in its literal original sense here (GiB), unlike its
	// generic-tier-boundary reuse in gcp_dns.go/gcp_kms.go.
	var totalCost float64
	haveEstimate := false
	if spec.ThroughputGBPerMonth != nil {
		var throughputTiers []egressTier
		if destination == "basic" {
			throughputTiers = []egressTier{
				{thresholdGB: 0, rate: 0, label: "free"},
				{thresholdGB: pubsubBasicFreeTierGB, rate: throughputRate, label: "paid"},
			}
		} else {
			throughputTiers = []egressTier{{thresholdGB: 0, rate: throughputRate, label: "flat"}}
		}
		cost, _ := addTieredEstimate(breakdown, "throughput", throughputTiers, *spec.ThroughputGBPerMonth)
		totalCost += cost
		haveEstimate = true
	}
	if spec.StorageGB != nil {
		storageTiers := []egressTier{{thresholdGB: 0, rate: storageRate, label: "flat"}}
		cost, _ := addTieredEstimate(breakdown, "storage", storageTiers, *spec.StorageGB)
		totalCost += cost
		haveEstimate = true
	}
	if haveEstimate {
		breakdown["monthly_cost"] = breakdownMoney(totalCost, "/mo")
	}

	return annotateFreshWithURL([]models.NormalizedPrice{*throughputPrice, *storagePrice}, pubsubSourceURL), breakdown, nil
}
