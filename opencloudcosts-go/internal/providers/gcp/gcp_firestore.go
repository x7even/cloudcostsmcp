// gcp_firestore.go — GCP Cloud Firestore pricing (domain=nosql,
// service=firestore).
//
// Cloud Firestore bills along several independent, PER-REGION dimensions:
//   - a per-GiB-month storage charge, with a genuine (verified live) free
//     allowance;
//   - per-operation read/write/delete charges, each with a genuine (verified
//     live) free allowance;
//   - a per-operation TTL (time-to-live) delete charge;
//   - a per-GiB-month point-in-time-recovery (PITR) storage charge;
//   - a per-GiB-month zonal backup storage charge;
//   - a per-GiB backup restore operation charge; and
//   - a per-GiB database clone operation charge.
//
// The last five (TTL deletes, PITR storage, zonal backup storage, backup
// restore, clone) each publish a SKU description variant containing
// "(with free tier)" that turns out, verified live, to carry byte-identical
// tiered rates to the plain variant — i.e. NO genuine free tier despite the
// naming. Do not assume "(with free tier)" in a description implies an
// actual $0 allowance; the tier structure itself (fetchFirestoreRates below)
// is what decides that, per SKU, every fetch.
//
// Verified live against the GCP Cloud Billing Catalog API (service
// EE2C-7FAC-5E08, "Cloud Firestore", 2072 SKUs total, fully paginated, two
// different page sizes cross-checked — see issue #80).
//
// REGION HANDLING — the one place this file diverges sharply from every
// other GCP pricing file added so far (Cloud DNS #78 / Cloud KMS #77 / Cloud
// Pub/Sub #79, all genuinely region-invariant):
//
//   - serviceRegions is ["global"] on EVERY Firestore SKU, regardless of
//     actual region-specificity. It is NOT usable for region matching here —
//     skuMatchesRegion (gcp.go), which matches against serviceRegions, would
//     match every region's SKU against any requested region. This file never
//     calls it.
//   - The authoritative region signal is geoTaxonomy.type ("REGIONAL" |
//     "MULTI_REGIONAL" | "GLOBAL") and geoTaxonomy.regions — parsed by
//     skuGeoTaxonomy below.
//   - REGIONAL SKUs key by their single geoTaxonomy.regions entry directly
//     (e.g. "us-east1").
//   - MULTI_REGIONAL SKUs (nam5, nam7, eur3) do NOT key cleanly by
//     geoTaxonomy.regions: nam5's constituents are
//     [us-central1, us-central2, us-east1] and nam7's are
//     [us-central1, us-central2, us-east4] — us-central1/us-central2 overlap
//     between the two multi-regions, so keying by (sorted) constituent list
//     would collide nam5 and nam7 rates into the same bucket. Instead, the
//     multi-region's own short name (nam5/nam7/eur3) is parsed directly out
//     of the SKU description and used as the map key — entirely distinct
//     from every regional key, by construction (no real GCP region is named
//     "nam5"/"nam7"/"eur3").
//   - GLOBAL SKUs found inside otherwise in-scope resourceGroups (the 4 CUD
//     metadata SKUs under FirestoreReadOps) are explicitly excluded — see
//     firestoreCategoryFor's caller in fetchFirestoreRates.
//
// The resulting map[string]firestoreRates is therefore a mix of ~41 regional
// keys ("us-central1", "us-east1", ...) and 3 multi-region keys ("nam5",
// "nam7", "eur3"), looked up by exact spec.Region string — which is exactly
// what FirestorePricingSpec.Region is expected to hold (a region code or one
// of the three multi-region names).
//
// FALLBACK STRATEGY for a region absent from the live map (unrecognized
// region, or the live fetch failed entirely): fall back to the published
// us-central1 rates (firestoreFallbackRates), NOT a full per-region fallback
// table — GCP publishes ~41 regions of genuinely different Firestore rates
// (issue #80's research: e.g. us-east4 is the cheapest tier at $0.099/GiB
// storage, while us-east1 and asia-east2 are tied for the most expensive
// tier at $0.18/GiB — region-code prefixes like "us-" are NOT a reliable
// proxy for price tier), which is too large and too likely to drift for a
// hardcoded Go table to be worth maintaining. Falling back to a single
// representative region is honest about the tradeoff: the response is
// flagged fallback=true with an explicit note naming the actual region
// requested and the region whose rates were substituted, rather than
// silently presenting a possibly-wrong regional rate with full confidence.
//
// Explicitly out of scope (deliberately, not gaps — see FirestorePricingSpec
// doc in models.go for the full rationale):
//   - Firestore Enterprise edition (resourceGroup DatastoreOps/
//     DatastoreBandwidth, or any description containing "Enterprise").
//   - The 4 GLOBAL CUD metadata SKUs under FirestoreReadOps.
//   - Firestore/Datastore network bandwidth and egress SKUs (resourceGroup
//     FirestoreBandwidth / DatastoreBandwidth) — deferred, same precedent as
//     Cloud Pub/Sub's egress SKUs (#79).
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

// firestoreServiceID is the GCP Cloud Billing Catalog service ID for
// "Cloud Firestore" — verified live 2026-07-06.
const firestoreServiceID = "EE2C-7FAC-5E08"

// firestoreSourceURL is the canonical public pricing page used for
// cross-checking and as the SourceURL stamped on returned prices.
const firestoreSourceURL = "https://cloud.google.com/firestore/pricing"

// firestoreDefaultRegion is used when a FirestorePricingSpec omits Region,
// mirroring the "us-central1" default convention used throughout
// gcp_networking.go/gcp_compute.go.
const firestoreDefaultRegion = "us-central1"

// firestoreMultiRegions is the ordered list of recognized GCP Firestore
// multi-region names, each matched against a SKU's description (see
// skuFirestoreMultiRegion). Order does not matter for correctness (the three
// literal names never overlap as substrings of one another) but is kept
// alphabetical for readability.
var firestoreMultiRegions = []string{"eur3", "nam5", "nam7"}

// firestoreRatesCacheKey caches the entire derived per-region rates map so
// repeated calls for different regions/quantities don't re-fetch/re-scan the
// full (2072-SKU) Firestore catalog.
const firestoreRatesCacheKey = "gcp:firestore:rates"

// firestoreRates holds one region's (or multi-region's) derived Cloud
// Firestore rates. A zero Rate field means no matching SKU was found for
// that bucket in that region and the caller should fall back to
// firestoreFallbackRates.
type firestoreRates struct {
	StorageRate        float64 `json:"storage_rate"`          // $/GiB-month
	StorageFreeGBMonth float64 `json:"storage_free_gb_month"` // GiB-months free before StorageRate applies (0 = no genuine free tier)
	ReadRate           float64 `json:"read_rate"`             // $/read operation
	ReadFreeOpsMonth   float64 `json:"read_free_ops_month"`   // read operations free per month (0 = no genuine free tier)
	WriteRate          float64 `json:"write_rate"`            // $/write (put) operation
	WriteFreeOpsMonth  float64 `json:"write_free_ops_month"`  // write operations free per month (0 = no genuine free tier)
	DeleteRate         float64 `json:"delete_rate"`           // $/delete operation
	DeleteFreeOpsMonth float64 `json:"delete_free_ops_month"` // delete operations free per month (0 = no genuine free tier)
	TTLDeleteRate      float64 `json:"ttl_delete_rate"`       // $/TTL delete operation — verified NO genuine free tier
	PITRStorageRate    float64 `json:"pitr_storage_rate"`     // $/GiB-month — verified NO genuine free tier
	ZonalBackupRate    float64 `json:"zonal_backup_rate"`     // $/GiB-month — verified NO genuine free tier
	RestoreRate        float64 `json:"restore_rate"`          // $/GiB restored — verified NO genuine free tier
	CloneRate          float64 `json:"clone_rate"`            // $/GiB cloned — verified NO genuine free tier
	SmallOpsRate       float64 `json:"small_ops_rate"`        // $/op — verified always $0 (informational only)
}

// firestoreFallbackRates holds the published, live-verified us-central1
// rates (issue #80) used when the live SKU catalog is unavailable, a
// description match fails, or the requested region is absent from the live
// map (see file header's FALLBACK STRATEGY section).
var firestoreFallbackRates = firestoreRates{
	StorageRate:        0.15,
	StorageFreeGBMonth: 1.0, // flat 1 GiB-month static allowance (verified: cloud.google.com/firestore/docs/quotas lists "Stored data: 1 GiB" with no "/day" qualifier, unlike the ops quotas below)
	ReadRate:           0.03 / 100000,
	ReadFreeOpsMonth:   1500000, // ~= 50,000 reads/day * 30 days, approximate monthly-equivalent
	WriteRate:          0.09 / 100000,
	WriteFreeOpsMonth:  600000, // ~= 20,000 writes/day * 30 days, approximate monthly-equivalent
	DeleteRate:         0.01 / 100000,
	DeleteFreeOpsMonth: 600000, // ~= 20,000 deletes/day * 30 days, approximate monthly-equivalent
	TTLDeleteRate:      0.01 / 100000,
	PITRStorageRate:    0.15,
	ZonalBackupRate:    0.03,
	RestoreRate:        0.20,
	CloneRate:          0.20,
	SmallOpsRate:       0,
}

// FirestoreValidQuantityFields lists every FirestorePricingSpec quantity
// field name (as it appears in the JSON request), used only by tests (in
// this package and in internal/tools) to keep the fillDomain collision audit
// honest as fields are added.
var FirestoreValidQuantityFields = []string{
	"storage_gb", "reads_per_month", "writes_per_month", "deletes_per_month",
	"ttl_deletes_per_month", "pitr_storage_gb", "zonal_backup_storage_gb",
	"restore_gb", "clone_gb",
}

// skuGeoTaxonomy extracts a raw GCP SKU's geoTaxonomy.type and
// geoTaxonomy.regions — the authoritative region signal for Cloud Firestore
// (see file header). Returns ("", nil) if geoTaxonomy is absent.
func skuGeoTaxonomy(sku map[string]any) (regionType string, regions []string) {
	geo, ok := sku["geoTaxonomy"].(map[string]any)
	if !ok {
		return "", nil
	}
	regionType, _ = geo["type"].(string)
	raw, _ := geo["regions"].([]any)
	for _, r := range raw {
		if s, ok := r.(string); ok && s != "" {
			regions = append(regions, s)
		}
	}
	return regionType, regions
}

// skuFirestoreMultiRegion returns the GCP multi-region short name (nam5,
// nam7, or eur3) found in a SKU's description, or "" if none matches. Used
// instead of geoTaxonomy.regions for MULTI_REGIONAL SKUs because the
// constituent-region lists overlap between nam5 and nam7 (see file header).
func skuFirestoreMultiRegion(descLower string) string {
	for _, mr := range firestoreMultiRegions {
		if strings.Contains(descLower, mr) {
			return mr
		}
	}
	return ""
}

// firestoreCategoryFor classifies a raw Cloud Firestore SKU by its
// category.resourceGroup (and, for the one ambiguous resourceGroup, its
// description), returning the internal bucket name and whether the SKU is
// in scope at all. Any description containing "enterprise" is rejected
// regardless of resourceGroup, as a defensive second check on top of the
// resourceGroup-level Enterprise exclusion (DatastoreOps/DatastoreBandwidth).
func firestoreCategoryFor(resourceGroup string, descLower string) (bucket string, inScope bool) {
	if strings.Contains(descLower, "enterprise") {
		return "", false
	}
	switch resourceGroup {
	case "FirestoreStorage":
		return "storage", true
	case "FirestorePITRStorage":
		return "pitr_storage", true
	case "FirestoreZonalBackupStorage":
		return "zonal_backup", true
	case "FirestoreSmallOps":
		return "small_ops", true
	case "FirestoreReadOps":
		return "read", true
	case "FirestoreEntityPutOps":
		return "write", true
	case "FirestoreEntityDeleteOps":
		return "delete", true
	case "FirestoreTtlDeleteOps":
		return "ttl_delete", true
	case "FirestoreRestoreOps":
		return "restore", true
	case "DatastoreOps":
		// DatastoreOps mixes Enterprise-edition operations (excluded above)
		// with Standard-edition "Database clone" operations (in scope) —
		// verified live, issue #80. Only the Clone SKUs are kept here.
		if strings.Contains(descLower, "clone") {
			return "clone", true
		}
		return "", false
	case "FirestoreBandwidth", "DatastoreBandwidth":
		// Network/egress — deliberately out of scope, see file header.
		return "", false
	default:
		return "", false
	}
}

// firestoreBucketRate derives a (rate, freeThreshold) pair from a SKU's own
// tier list:
//   - 1 tier: flat rate, no free tier.
//   - >=2 tiers whose first tier starts at 0 and is priced at $0: a genuine
//     free tier (freeThreshold = second tier's startUsageAmount), rate =
//     last tier's price.
//   - >=2 tiers otherwise (e.g. the "(with free tier)" description variants
//     that turn out to carry byte-identical rates — TTL deletes, PITR
//     storage, zonal backup storage, restore, clone, verified live): no
//     genuine free tier, rate = last tier's price.
//
// This single rule handles both the real-free-tier categories (storage,
// read, write, delete) and the misleadingly-named-but-flat categories (TTL
// deletes, PITR storage, zonal backup storage, restore, clone) without
// needing to special-case any of them: the tier *structure* — not the
// "(with free tier)" wording — decides whether a free allowance is genuine.
func firestoreBucketRate(sku map[string]any) (rate float64, freeThreshold float64) {
	tiers := skuTierList(sku)
	if len(tiers) == 0 {
		return 0, 0
	}
	if len(tiers) > 2 {
		// Today's catalog is verified 2-tier-max for every in-scope Firestore
		// bucket. A 3rd+ tier would mean a graduated-rate structure this
		// function doesn't model (it always takes the LAST tier's price as
		// the flat paid rate and the 2nd tier's start as the free
		// threshold), which would silently mis-price. Warn rather than
		// implementing full graduated-tier support for a case that has never
		// been observed live.
		desc, _ := sku["description"].(string)
		slog.Warn("gcp firestore: SKU has more than 2 tiers; firestoreBucketRate assumes at most 2 and may mis-price", "description", desc, "tier_count", len(tiers))
	}
	rate = tiers[len(tiers)-1].price
	if len(tiers) >= 2 && tiers[0].start == 0 && tiers[0].price == 0 {
		freeThreshold = tiers[1].start
	}
	return rate, freeThreshold
}

// firestoreRegionKey lowercases a region string for use as a rates-map key,
// so lookups are case-insensitive regardless of how the caller (or a SKU's
// geoTaxonomy.regions entry) capitalizes the region — see
// gcp_networking.go's strings.ToLower(region) for the precedent.
func firestoreRegionKey(region string) string {
	return strings.ToLower(region)
}

// fetchFirestoreRates returns the live, derived per-region Cloud Firestore
// rates, caching the result. The returned map's keys are a mix of ~41
// regional codes and 3 multi-region names (nam5/nam7/eur3) — see file
// header. A missing key, or a zero Rate field within a present key, means no
// matching SKU was found and the caller should fall back to
// firestoreFallbackRates.
//
// Matching prefers the "(with free tier)" description variant over the
// plain variant for a given (region, bucket) pair — see firestoreBucketRate
// for why the free-tier variant alone is sufficient to correctly derive both
// the genuine-free-tier and no-free-tier cases. If only the plain variant is
// found (or the two disagree unexpectedly), the first-matched SKU wins and
// later matches are logged and discarded, mirroring gcp_dns.go/
// gcp_pubsub.go's dedup convention.
func (p *Provider) fetchFirestoreRates(ctx context.Context) map[string]firestoreRates {
	if raw, ok := p.cache.GetMetadata(firestoreRatesCacheKey); ok {
		var r map[string]firestoreRates
		if err := json.Unmarshal(raw, &r); err == nil {
			return r
		}
	}

	rates := make(map[string]firestoreRates)
	skus, err := p.fetchSKUs(ctx, firestoreServiceID)
	if err != nil {
		slog.Warn("gcp firestore: fetch SKUs failed", "err", err)
		return rates
	}

	// isFreeVariant[region][bucket] tracks whether the value currently
	// stored for (region, bucket) came from a "(with free tier)" SKU
	// description, so a later plain-variant match for the same
	// (region, bucket) never overwrites a free-tier-variant match — see
	// firestoreBucketRate's doc for why the free-tier variant is preferred.
	isFreeVariant := make(map[string]map[string]bool)

	for _, sku := range skus {
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)

		cat, _ := sku["category"].(map[string]any)
		var resourceGroup string
		if cat != nil {
			resourceGroup, _ = cat["resourceGroup"].(string)
		}

		bucket, inScope := firestoreCategoryFor(resourceGroup, descLower)
		if !inScope {
			continue
		}

		regionType, geoRegions := skuGeoTaxonomy(sku)
		var key string
		switch regionType {
		case "REGIONAL":
			if len(geoRegions) != 1 {
				slog.Warn("gcp firestore: REGIONAL SKU without exactly one geoTaxonomy region; skipping", "description", desc, "regions", geoRegions)
				continue
			}
			key = firestoreRegionKey(geoRegions[0])
		case "MULTI_REGIONAL":
			key = skuFirestoreMultiRegion(descLower)
			if key == "" {
				slog.Warn("gcp firestore: MULTI_REGIONAL SKU with no recognized multi-region name in description; skipping", "description", desc)
				continue
			}
			key = firestoreRegionKey(key)
		default:
			// GLOBAL (the 4 CUD metadata SKUs under FirestoreReadOps) or
			// unrecognized/absent geoTaxonomy — not a genuine per-region
			// Firestore rate, skip.
			continue
		}

		hasFreeTierDesc := strings.Contains(descLower, "free tier")
		rate, freeThreshold := firestoreBucketRate(sku)
		// small_ops is verified always $0 with no free-tier tiering, so its
		// (rate, freeThreshold) is legitimately (0, 0) — the same zero pair
		// this guard otherwise treats as "no match found". Skipping the
		// guard for small_ops is what lets a matched small_ops SKU actually
		// reach the switch below and set r.SmallOpsRate; without this
		// exception every small_ops SKU is silently discarded here before
		// ever being applied.
		if bucket != "small_ops" && rate == 0 && freeThreshold == 0 {
			continue
		}

		if isFreeVariant[key] == nil {
			isFreeVariant[key] = make(map[string]bool)
		}
		already, seen := isFreeVariant[key][bucket]
		if seen && (already || !hasFreeTierDesc) {
			// Either a free-tier-variant match already won this bucket
			// (kept), or both this and the existing match are plain
			// variants (first match wins) — either way, do not overwrite.
			//
			// A free variant beating a later plain variant for the same
			// bucket (already=true, hasFreeTierDesc=false) is the normal,
			// expected shape of the real catalog — every bucket ships both
			// a plain and a "(with free tier)" SKU — so warning here would
			// just be routine log spam on well-formed data, which is the
			// opposite of what gcp_pubsub.go/gcp_dns.go's convention
			// intends (warn only when something is unexpected). Only warn
			// when two SKUs of the *same* kind (both free-tier variants, or
			// both plain) matched the same bucket, since that is the
			// genuinely surprising case this guard exists to catch.
			if already == hasFreeTierDesc {
				slog.Warn("gcp firestore: multiple SKUs matched bucket; keeping first match", "region", key, "bucket", bucket, "description", desc)
			}
			continue
		}
		isFreeVariant[key][bucket] = hasFreeTierDesc

		r := rates[key]
		switch bucket {
		case "storage":
			r.StorageRate, r.StorageFreeGBMonth = rate, freeThreshold
		case "read":
			r.ReadRate, r.ReadFreeOpsMonth = rate, freeThreshold
		case "write":
			r.WriteRate, r.WriteFreeOpsMonth = rate, freeThreshold
		case "delete":
			r.DeleteRate, r.DeleteFreeOpsMonth = rate, freeThreshold
		case "ttl_delete":
			r.TTLDeleteRate = rate
		case "pitr_storage":
			r.PITRStorageRate = rate
		case "zonal_backup":
			r.ZonalBackupRate = rate
		case "restore":
			r.RestoreRate = rate
		case "clone":
			r.CloneRate = rate
		case "small_ops":
			r.SmallOpsRate = rate
		}
		rates[key] = r
	}

	if raw, err := json.Marshal(rates); err == nil {
		p.cache.SetMetadata(firestoreRatesCacheKey, raw, p.cfg.MetadataTTL())
	}
	return rates
}

// pickRateFreeThreshold resolves a (Rate, FreeThreshold) field pair —
// storage, read, write, and delete each have one. pickRate's "live==0 means
// missing" heuristic is safe applied to Rate (every in-scope Firestore rate
// is genuinely non-zero, except SmallOpsRate, handled separately in
// resolveFirestoreRates) but is NOT safe applied directly to FreeThreshold:
// a bucket can be live-matched (Rate present) with a genuine free threshold
// of zero, and pickRate would incorrectly discard that genuine zero and
// substitute the fallback threshold instead. So FreeThreshold's fallback
// substitution is gated on Rate's own fallback decision, not on
// FreeThreshold's value: only when the bucket is entirely unmatched live
// (Rate fell back) does FreeThreshold also fall back; otherwise the live
// FreeThreshold is trusted verbatim, including when it is zero.
func pickRateFreeThreshold(liveRate, fallbackRate, liveFree, fallbackFree float64) (rate, free float64, usedFallback bool) {
	rate, usedFallback = pickRate(liveRate, fallbackRate)
	if usedFallback {
		return rate, fallbackFree, true
	}
	return rate, liveFree, false
}

// resolveFirestoreRates picks the rates bucket to use for region: the live
// bucket if present and non-empty, otherwise firestoreFallbackRates — see
// file header's FALLBACK STRATEGY section for why a single representative
// fallback (us-central1) is used instead of a full per-region table.
// usedFallback is true whenever ANY field fell back (per-field pickRate),
// which includes both the "region entirely absent from the live map" case
// and the "region present but one bucket's SKU wasn't matched" case.
func resolveFirestoreRates(live map[string]firestoreRates, region string) (rates firestoreRates, usedFallback bool) {
	l, ok := live[region]
	fb := firestoreFallbackRates
	if !ok {
		usedFallback = true
	}

	// Plain rate fields: a single live value, no paired free-threshold.
	simplePairs := []struct {
		live *float64
		fb   float64
		out  *float64
	}{
		{&l.TTLDeleteRate, fb.TTLDeleteRate, &rates.TTLDeleteRate},
		{&l.PITRStorageRate, fb.PITRStorageRate, &rates.PITRStorageRate},
		{&l.ZonalBackupRate, fb.ZonalBackupRate, &rates.ZonalBackupRate},
		{&l.RestoreRate, fb.RestoreRate, &rates.RestoreRate},
		{&l.CloneRate, fb.CloneRate, &rates.CloneRate},
	}
	for _, p := range simplePairs {
		var f bool
		*p.out, f = pickRate(*p.live, p.fb)
		usedFallback = usedFallback || f
	}

	// Rate + FreeThreshold pairs: see pickRateFreeThreshold's doc for why
	// these can't use the same plain pickRate loop above.
	freeTieredPairs := []struct {
		liveRate, fbRate float64
		liveFree, fbFree float64
		outRate, outFree *float64
	}{
		{l.StorageRate, fb.StorageRate, l.StorageFreeGBMonth, fb.StorageFreeGBMonth, &rates.StorageRate, &rates.StorageFreeGBMonth},
		{l.ReadRate, fb.ReadRate, l.ReadFreeOpsMonth, fb.ReadFreeOpsMonth, &rates.ReadRate, &rates.ReadFreeOpsMonth},
		{l.WriteRate, fb.WriteRate, l.WriteFreeOpsMonth, fb.WriteFreeOpsMonth, &rates.WriteRate, &rates.WriteFreeOpsMonth},
		{l.DeleteRate, fb.DeleteRate, l.DeleteFreeOpsMonth, fb.DeleteFreeOpsMonth, &rates.DeleteRate, &rates.DeleteFreeOpsMonth},
	}
	for _, p := range freeTieredPairs {
		var f bool
		*p.outRate, *p.outFree, f = pickRateFreeThreshold(p.liveRate, p.fbRate, p.liveFree, p.fbFree)
		usedFallback = usedFallback || f
	}

	// SmallOpsRate is always $0 (verified live) — pickRate would treat a
	// genuine live $0 as "missing" and substitute the (also $0) fallback,
	// which is harmless here but is not the reason usedFallback is computed
	// from every other field above; SmallOpsRate is intentionally excluded
	// from the usedFallback computation.
	rates.SmallOpsRate = fb.SmallOpsRate
	if l.SmallOpsRate != 0 {
		rates.SmallOpsRate = l.SmallOpsRate
	}

	return rates, usedFallback
}

// newFirestorePrice builds a Cloud Firestore NormalizedPrice tagged with the
// requested region (NOT global scope — see file header) and the fields
// common to every Firestore line item.
func newFirestorePrice(region, skuID, description string, pricePerUnit float64, unit models.PriceUnit, attrs map[string]string) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "firestore",
		SKUID:         skuID,
		ProductFamily: "Cloud Firestore",
		Description:   description,
		Region:        region,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerUnit,
		Unit:          unit,
		Currency:      "USD",
		Attributes:    attrs,
	}
}

// priceFirestore returns Cloud Firestore pricing for the given
// FirestorePricingSpec: one NormalizedPrice per billing dimension (storage,
// read, write, delete, TTL delete, PITR storage, zonal backup storage,
// restore, clone, small ops), always returned together — mirroring the
// multi-line-item shape used by Cloud DNS (zone+query) and Cloud Pub/Sub
// (throughput+storage).
//
// The 9 "dimension" strings stamped below via attrs(...) are independently
// re-enumerated in internal/tools/lookup.go's normalizedPriceSummary
// (the `case firestoreSpec != nil:` switch on Attributes["dimension"]),
// which maps each one to its FirestorePricingSpec quantity field. This is
// the same duplication tension already accepted for Cloud Pub/Sub's
// destination handling (see pubsubDestinations' doc comment) and is left
// unconsolidated here for the same reason: lookup.go is a provider-agnostic
// dispatch layer, and threading a shared dimension-name table across the
// gcp/tools package boundary purely to save re-typing 9 string literals was
// judged not worth the added indirection for a fixed, rarely-changing list.
func (p *Provider) priceFirestore(
	ctx context.Context,
	spec *models.FirestorePricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	region := firestoreRegionKey(spec.Region)
	if region == "" {
		region = firestoreRegionKey(p.DefaultRegion())
	}

	live := p.fetchFirestoreRates(ctx)
	_, regionFound := live[region]
	// A region is only "unrecognized" when the live fetch itself succeeded
	// (len(live) > 0) but this specific region key is absent from it. If the
	// fetch failed entirely (empty catalog, network error, etc. — see
	// fetchFirestoreRates), EVERY region is absent from an empty map, and
	// that is a fetch failure, not a signal that this particular region is
	// unrecognized by GCP.
	regionUnrecognized := len(live) > 0 && !regionFound
	rates, usedFallback := resolveFirestoreRates(live, region)

	// attrs stamps a "dimension" attribute distinguishing each line item —
	// necessary because several dimensions share the same PriceUnit
	// (storage/pitr_storage/zonal_backup are all PriceUnitPerGBMonth;
	// read/write/delete/ttl_delete/small_ops are all PriceUnitPerOperation),
	// so PriceUnit alone cannot disambiguate them the way it does for
	// DNSPricingSpec/PubSubPricingSpec in normalizedPriceSummary
	// (internal/tools/lookup.go). Each call returns a fresh map so no two
	// NormalizedPrices share a mutable Attributes reference.
	attrs := func(dimension string) map[string]string {
		return map[string]string{"dimension": dimension}
	}

	prices := []models.NormalizedPrice{
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:storage:%s", region),
			"Cloud Firestore Storage (paid rate; free allowance applies first)",
			rates.StorageRate, models.PriceUnitPerGBMonth, attrs("storage")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:read:%s", region),
			"Cloud Firestore Entity Read Operations (paid rate; free allowance applies first)",
			rates.ReadRate, models.PriceUnitPerOperation, attrs("read")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:write:%s", region),
			"Cloud Firestore Entity Write (Put) Operations (paid rate; free allowance applies first)",
			rates.WriteRate, models.PriceUnitPerOperation, attrs("write")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:delete:%s", region),
			"Cloud Firestore Entity Delete Operations (paid rate; free allowance applies first)",
			rates.DeleteRate, models.PriceUnitPerOperation, attrs("delete")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:ttl_delete:%s", region),
			"Cloud Firestore TTL Delete Operations (no genuine free tier — verified live)",
			rates.TTLDeleteRate, models.PriceUnitPerOperation, attrs("ttl_delete")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:pitr_storage:%s", region),
			"Cloud Firestore Point-in-Time Recovery (PITR) Storage (no genuine free tier — verified live)",
			rates.PITRStorageRate, models.PriceUnitPerGBMonth, attrs("pitr_storage")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:zonal_backup:%s", region),
			"Cloud Firestore Zonal Backup Storage (no genuine free tier — verified live)",
			rates.ZonalBackupRate, models.PriceUnitPerGBMonth, attrs("zonal_backup")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:restore:%s", region),
			"Cloud Firestore Backup Restore Operation (no genuine free tier — verified live)",
			rates.RestoreRate, models.PriceUnitPerGB, attrs("restore")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:clone:%s", region),
			"Cloud Firestore Database Clone Operation (no genuine free tier — verified live)",
			rates.CloneRate, models.PriceUnitPerGB, attrs("clone")),
		newFirestorePrice(region, fmt.Sprintf("gcp:firestore:small_ops:%s", region),
			"Cloud Firestore Small Operations (verified live: always $0.00 regardless of volume)",
			rates.SmallOpsRate, models.PriceUnitPerOperation, attrs("small_ops")),
	}

	breakdown := map[string]any{
		"region":                    region,
		"storage_rate":              breakdownMoney(rates.StorageRate, "/GiB-month"),
		"storage_free_gb_month":     rates.StorageFreeGBMonth,
		"read_rate":                 breakdownMoney(rates.ReadRate, "/read operation"),
		"read_free_ops_per_month":   rates.ReadFreeOpsMonth,
		"write_rate":                breakdownMoney(rates.WriteRate, "/write operation"),
		"write_free_ops_per_month":  rates.WriteFreeOpsMonth,
		"delete_rate":               breakdownMoney(rates.DeleteRate, "/delete operation"),
		"delete_free_ops_per_month": rates.DeleteFreeOpsMonth,
		"ttl_delete_rate":           breakdownMoney(rates.TTLDeleteRate, "/TTL delete operation"),
		"pitr_storage_rate":         breakdownMoney(rates.PITRStorageRate, "/GiB-month"),
		"zonal_backup_rate":         breakdownMoney(rates.ZonalBackupRate, "/GiB-month"),
		"restore_rate":              breakdownMoney(rates.RestoreRate, "/GiB restored"),
		"clone_rate":                breakdownMoney(rates.CloneRate, "/GiB cloned"),
		"small_ops_rate":            breakdownMoney(rates.SmallOpsRate, "/operation"),
		"no_free_tier_note":         "ttl_delete, pitr_storage, zonal_backup, restore, and clone have NO genuine free tier despite GCP publishing a '(with free tier)' SKU description variant for each — verified live that both description variants carry byte-identical tiered rates.",
	}
	if regionUnrecognized {
		breakdown["region_unrecognized"] = true
		breakdown["region_unrecognized_note"] = fmt.Sprintf(
			"region %q was not found in the live Cloud Firestore SKU catalog; substituted %s rates as an approximation. Verify actual rates for this region at %s — Firestore rates vary significantly by region and are NOT well-approximated by any single other region.",
			region, firestoreDefaultRegion, firestoreSourceURL,
		)
	}
	if usedFallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rate(s) for one or more dimensions; live SKU catalog unavailable or returned no match for this region. Verify current rates at " + firestoreSourceURL + "."
	}

	var totalCost float64
	haveEstimate := false
	addFlat := func(key string, rate float64, quantity *float64) {
		if quantity == nil {
			return
		}
		cost := rate * (*quantity)
		breakdown[key+"_monthly_cost"] = breakdownMoney(cost, "/mo")
		totalCost += cost
		haveEstimate = true
	}
	addFreeTiered := func(key string, rate float64, freeThreshold float64, quantity *float64) {
		if quantity == nil {
			return
		}
		tiers := []egressTier{
			{thresholdGB: 0, rate: 0, label: "free"},
			{thresholdGB: freeThreshold, rate: rate, label: "paid"},
		}
		cost, _ := addTieredEstimate(breakdown, key, tiers, *quantity)
		totalCost += cost
		haveEstimate = true
	}

	addFreeTiered("storage", rates.StorageRate, rates.StorageFreeGBMonth, spec.StorageGB)
	addFreeTiered("read", rates.ReadRate, rates.ReadFreeOpsMonth, spec.ReadsPerMonth)
	addFreeTiered("write", rates.WriteRate, rates.WriteFreeOpsMonth, spec.WritesPerMonth)
	addFreeTiered("delete", rates.DeleteRate, rates.DeleteFreeOpsMonth, spec.DeletesPerMonth)
	addFlat("ttl_delete", rates.TTLDeleteRate, spec.TTLDeletesPerMonth)
	addFlat("pitr_storage", rates.PITRStorageRate, spec.PITRStorageGB)
	addFlat("zonal_backup", rates.ZonalBackupRate, spec.ZonalBackupStorageGB)
	addFlat("restore", rates.RestoreRate, spec.RestoreGB)
	addFlat("clone", rates.CloneRate, spec.CloneGB)

	if haveEstimate {
		breakdown["monthly_cost"] = breakdownMoney(totalCost, "/mo")
	}

	return annotateFreshWithURL(prices, firestoreSourceURL), breakdown, nil
}
