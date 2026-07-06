// Package gcp — unit tests for Cloud Firestore pricing (issue #80).
//
// SKU descriptions/rates/SKU IDs referenced in these fixtures come from the
// live-catalog research report for issue #80 (Cloud Billing Catalog API,
// serviceId EE2C-7FAC-5E08, "Cloud Firestore", 2072 SKUs total, fully
// paginated). Tier startUsageAmount boundaries for the free-tier fixtures use
// round numbers consistent with the report's approximate monthly-equivalent
// free allowances (see firestoreFallbackRates in gcp_firestore.go) rather than
// GCP's exact (undisclosed) tier boundary encoding, since only the *behavior*
// of the tier-derivation logic is under test, not a byte-exact catalog replay.
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
// Firestore SKU fixture helpers
// --------------------------------------------------------------------------

// firestoreTier is one (startUsageAmount, units, nanos) tier used to build a
// fixture SKU's pricingInfo[0].pricingExpression.tieredRates.
type firestoreTier struct {
	start float64
	units string
	nanos int
}

// firestoreSKU builds a raw Cloud Firestore SKU fixture. geoType is
// "REGIONAL", "MULTI_REGIONAL", or "GLOBAL"; geoRegions is only meaningful
// for "REGIONAL" (Cloud Firestore's MULTI_REGIONAL SKUs are keyed by parsing
// the multi-region name out of the description instead — see
// skuFirestoreMultiRegion).
func firestoreSKU(desc, resourceGroup, geoType string, geoRegions []string, tiers []firestoreTier) map[string]any {
	tieredRates := make([]any, len(tiers))
	for i, t := range tiers {
		tieredRates[i] = map[string]any{
			"startUsageAmount": t.start,
			"unitPrice": map[string]any{
				"units": t.units,
				"nanos": float64(t.nanos),
			},
		}
	}
	regions := make([]any, len(geoRegions))
	for i, r := range geoRegions {
		regions[i] = r
	}
	return map[string]any{
		"description": desc,
		// serviceRegions is deliberately ["global"] on every real Firestore
		// SKU (see gcp_firestore.go file header) — included here so any
		// accidental future use of skuMatchesRegion against this fixture
		// would silently "succeed" for every region, the exact trap this
		// file's code must not fall into.
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": resourceGroup,
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type":    geoType,
			"regions": regions,
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": tieredRates,
				},
			},
		},
	}
}

// firestoreSKUFlat builds a single-tier SKU: no free allowance.
func firestoreSKUFlat(desc, resourceGroup, geoType string, geoRegions []string, units string, nanos int) map[string]any {
	return firestoreSKU(desc, resourceGroup, geoType, geoRegions, []firestoreTier{
		{start: 0, units: units, nanos: nanos},
	})
}

// firestoreSKUFreeTier builds a genuine two-tier free-then-paid SKU: tier1
// (free) at startUsageAmount=0 priced $0, tier2 (paid) at startUsageAmount
// threshold.
func firestoreSKUFreeTier(desc, resourceGroup, geoType string, geoRegions []string, threshold float64, units string, nanos int) map[string]any {
	return firestoreSKU(desc, resourceGroup, geoType, geoRegions, []firestoreTier{
		{start: 0, units: "0", nanos: 0},
		{start: threshold, units: units, nanos: nanos},
	})
}

// firestoreSKUFakeFreeTierVariant builds a two-tier SKU whose description
// contains "(with free tier)" but whose tiers carry the SAME (byte-identical)
// rate throughout — modeling the verified-live TTL-delete/PITR-storage/
// zonal-backup/restore/clone SKUs, where the "(with free tier)" description
// variant does NOT actually grant a free allowance. Proves
// firestoreBucketRate derives "no genuine free tier" from tier *structure*,
// not the description wording.
func firestoreSKUFakeFreeTierVariant(desc, resourceGroup, geoType string, geoRegions []string, threshold float64, units string, nanos int) map[string]any {
	return firestoreSKU(desc, resourceGroup, geoType, geoRegions, []firestoreTier{
		{start: 0, units: units, nanos: nanos},
		{start: threshold, units: units, nanos: nanos},
	})
}

// firestoreSKUResponse wraps Firestore SKUs for the httptest server.
func firestoreSKUResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// fakeFirestoreSKUsForRegion returns one fake SKU per rate bucket for a
// single REGIONAL Firestore region, at the live-verified us-central1 rates
// (issue #80): storage $0.15/GiB-mo (free 1.0 GiBy.mo), reads
// $0.03/100K (free 1.5M/mo), writes $0.09/100K (free 600K/mo), deletes
// $0.01/100K (free 600K/mo), small ops always $0, TTL deletes $0.01/100K (NO
// genuine free tier), PITR storage $0.15/GiB-mo (NO genuine free tier), zonal
// backup storage $0.03/GiB-mo (NO genuine free tier), restore $0.20/GiB (NO
// genuine free tier), clone $0.20/GiB (NO genuine free tier, verified live
// under the Enterprise/Datastore "DatastoreOps" resourceGroup despite being a
// Standard-edition operation).
func fakeFirestoreSKUsForRegion(region string) []map[string]any {
	regions := []string{region}
	return []map[string]any{
		// Real catalog SKU ID: 140B-ADDF-6A12 (plain) / F845-5E55-0738 (free-tier variant, used here).
		firestoreSKUFreeTier("Cloud Firestore Storage (with free tier)", "FirestoreStorage", "REGIONAL", regions, 1.0, "0", 150000000),
		// Real catalog SKU ID: 6A94-8525-876F (plain) / F251-5791-CE45 (free-tier variant, used here).
		firestoreSKUFreeTier("Cloud Firestore Entity Read Operations (with free tier)", "FirestoreReadOps", "REGIONAL", regions, 1500000, "0", 300),
		// Real catalog SKU ID: BFCC-1D11-14E1 (plain) / 63B3-E146-F0E9 (free-tier variant, used here).
		firestoreSKUFreeTier("Cloud Firestore Entity Write Operations (with free tier)", "FirestoreEntityPutOps", "REGIONAL", regions, 600000, "0", 900),
		// Real catalog SKU ID: B813-E6E7-37F4 (plain) / A16C-85B5-D0D0 (free-tier variant, used here).
		firestoreSKUFreeTier("Cloud Firestore Entity Delete Operations (with free tier)", "FirestoreEntityDeleteOps", "REGIONAL", regions, 600000, "0", 100),
		// Real catalog SKU ID: EBD9-E554-2C8E (plain) / 81F7-9373-FA24 (free-tier variant). Always $0.
		firestoreSKUFlat("Cloud Firestore Small Operations", "FirestoreSmallOps", "REGIONAL", regions, "0", 0),
		// Real catalog SKU ID: 6088-280E-4225 (plain, used here) / 912E-6A61-9F29 (misleading "(with free tier)" variant).
		firestoreSKUFlat("Cloud Firestore TTL Delete Operations", "FirestoreTtlDeleteOps", "REGIONAL", regions, "0", 100),
		// Real catalog SKU ID: A979-304C-860F (plain, used here) / 9AC7-D7C1-66E7 (misleading "(with free tier)" variant).
		firestoreSKUFlat("Cloud Firestore PITR Storage", "FirestorePITRStorage", "REGIONAL", regions, "0", 150000000),
		// Real catalog SKU ID: 1462-48DB-955E (plain, used here) / CFBB-542F-A2DE (misleading "(with free tier)" variant).
		firestoreSKUFlat("Cloud Firestore Zonal Backup Storage", "FirestoreZonalBackupStorage", "REGIONAL", regions, "0", 30000000),
		// Real catalog SKU ID: 5296-9E3F-58C7 (plain, used here) / 2F3B-786F-4F74 (misleading "(with free tier)" variant).
		firestoreSKUFlat("Cloud Firestore Backup Restore Operation", "FirestoreRestoreOps", "REGIONAL", regions, "0", 200000000),
		// Real catalog SKU ID: E801-ED1E-41ED (plain, used here) / 204C-018C-3F94 (misleading "(with free tier)" variant).
		// Verified live under DatastoreOps (Enterprise/Datastore resourceGroup) despite being Standard edition.
		firestoreSKUFlat("Cloud Firestore Database Clone Operation", "DatastoreOps", "REGIONAL", regions, "0", 200000000),
	}
}

// --------------------------------------------------------------------------
// firestoreCategoryFor
// --------------------------------------------------------------------------

func TestFirestoreCategoryFor(t *testing.T) {
	cases := []struct {
		name          string
		resourceGroup string
		desc          string
		wantBucket    string
		wantInScope   bool
	}{
		{"storage", "FirestoreStorage", "cloud firestore storage", "storage", true},
		{"pitr_storage", "FirestorePITRStorage", "cloud firestore pitr storage", "pitr_storage", true},
		{"zonal_backup", "FirestoreZonalBackupStorage", "cloud firestore zonal backup storage", "zonal_backup", true},
		{"small_ops", "FirestoreSmallOps", "cloud firestore small operations", "small_ops", true},
		{"read", "FirestoreReadOps", "cloud firestore entity read operations", "read", true},
		{"write", "FirestoreEntityPutOps", "cloud firestore entity write operations", "write", true},
		{"delete", "FirestoreEntityDeleteOps", "cloud firestore entity delete operations", "delete", true},
		{"ttl_delete", "FirestoreTtlDeleteOps", "cloud firestore ttl delete operations", "ttl_delete", true},
		{"restore", "FirestoreRestoreOps", "cloud firestore backup restore operation", "restore", true},
		{"datastore_clone", "DatastoreOps", "cloud firestore database clone operation", "clone", true},
		{"datastore_enterprise_excluded", "DatastoreOps", "enterprise edition write operation", "", false},
		{"datastore_enterprise_wins_over_clone_substring", "DatastoreOps", "clone operation (enterprise edition)", "", false},
		{"firestore_bandwidth_excluded", "FirestoreBandwidth", "cloud firestore network egress", "", false},
		{"datastore_bandwidth_excluded", "DatastoreBandwidth", "cloud datastore network egress", "", false},
		{"unknown_resource_group", "SomeUnknownGroup", "mystery sku", "", false},
		{"enterprise_overrides_known_group", "FirestoreStorage", "cloud firestore storage (enterprise edition)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bucket, inScope := firestoreCategoryFor(c.resourceGroup, c.desc)
			if bucket != c.wantBucket || inScope != c.wantInScope {
				t.Errorf("firestoreCategoryFor(%q, %q) = (%q, %v), want (%q, %v)",
					c.resourceGroup, c.desc, bucket, inScope, c.wantBucket, c.wantInScope)
			}
		})
	}
}

// --------------------------------------------------------------------------
// skuGeoTaxonomy / skuFirestoreMultiRegion
// --------------------------------------------------------------------------

func TestSkuGeoTaxonomy(t *testing.T) {
	regional := map[string]any{
		"geoTaxonomy": map[string]any{
			"type":    "REGIONAL",
			"regions": []any{"us-east4"},
		},
	}
	regionType, regions := skuGeoTaxonomy(regional)
	if regionType != "REGIONAL" || len(regions) != 1 || regions[0] != "us-east4" {
		t.Errorf("skuGeoTaxonomy(regional) = (%q, %v), want (\"REGIONAL\", [us-east4])", regionType, regions)
	}

	multiRegional := map[string]any{
		"geoTaxonomy": map[string]any{
			"type":    "MULTI_REGIONAL",
			"regions": []any{"us-central1", "us-central2", "us-east1"},
		},
	}
	regionType, regions = skuGeoTaxonomy(multiRegional)
	if regionType != "MULTI_REGIONAL" || len(regions) != 3 {
		t.Errorf("skuGeoTaxonomy(multiRegional) = (%q, %v), want (\"MULTI_REGIONAL\", 3 regions)", regionType, regions)
	}

	missing := map[string]any{}
	regionType, regions = skuGeoTaxonomy(missing)
	if regionType != "" || regions != nil {
		t.Errorf("skuGeoTaxonomy(missing) = (%q, %v), want (\"\", nil)", regionType, regions)
	}
}

func TestSkuFirestoreMultiRegion(t *testing.T) {
	cases := []struct {
		desc string
		want string
	}{
		{"cloud firestore storage (nam5)", "nam5"},
		{"cloud firestore storage (nam7)", "nam7"},
		{"cloud firestore storage (eur3)", "eur3"},
		{"cloud firestore storage (us-central1)", ""},
	}
	for _, c := range cases {
		if got := skuFirestoreMultiRegion(c.desc); got != c.want {
			t.Errorf("skuFirestoreMultiRegion(%q) = %q, want %q", c.desc, got, c.want)
		}
	}
}

// --------------------------------------------------------------------------
// firestoreBucketRate
// --------------------------------------------------------------------------

func TestFirestoreBucketRate_NoTiers(t *testing.T) {
	sku := map[string]any{}
	rate, free := firestoreBucketRate(sku)
	if rate != 0 || free != 0 {
		t.Errorf("firestoreBucketRate(no tiers) = (%v, %v), want (0, 0)", rate, free)
	}
}

func TestFirestoreBucketRate_FlatSingleTier(t *testing.T) {
	sku := firestoreSKUFlat("TTL delete", "FirestoreTtlDeleteOps", "REGIONAL", []string{"us-central1"}, "0", 100)
	rate, free := firestoreBucketRate(sku)
	if abs(rate-0.0000001) > 1e-12 {
		t.Errorf("rate = %.10f, want 0.0000001000", rate)
	}
	if free != 0 {
		t.Errorf("freeThreshold = %v, want 0 (single tier: no free allowance)", free)
	}
}

func TestFirestoreBucketRate_GenuineFreeTier(t *testing.T) {
	sku := firestoreSKUFreeTier("Storage (with free tier)", "FirestoreStorage", "REGIONAL", []string{"us-central1"}, 1.0, "0", 150000000)
	rate, free := firestoreBucketRate(sku)
	if abs(rate-0.15) > 1e-9 {
		t.Errorf("rate = %.6f, want 0.150000", rate)
	}
	if abs(free-1.0) > 1e-9 {
		t.Errorf("freeThreshold = %.6f, want 1.0 (genuine free tier)", free)
	}
}

// TestFirestoreBucketRate_MisleadingFreeTierNaming verifies that a
// "(with free tier)" description whose two tiers carry the SAME rate
// (byte-identical, as verified live for TTL deletes/PITR storage/zonal
// backup storage/restore/clone) derives NO free tier — the wording must not
// be trusted, only the tier structure.
func TestFirestoreBucketRate_MisleadingFreeTierNaming(t *testing.T) {
	sku := firestoreSKUFakeFreeTierVariant("TTL Delete Operations (with free tier)", "FirestoreTtlDeleteOps", "REGIONAL", []string{"us-central1"}, 600000, "0", 100)
	rate, free := firestoreBucketRate(sku)
	if abs(rate-0.0000001) > 1e-12 {
		t.Errorf("rate = %.10f, want 0.0000001000", rate)
	}
	if free != 0 {
		t.Errorf("freeThreshold = %v, want 0 (misleading naming: tiers carry the same rate, no genuine free allowance)", free)
	}
}

// --------------------------------------------------------------------------
// fetchFirestoreRates
// --------------------------------------------------------------------------

func TestFetchFirestoreRates_ParsesAllBucketsForOneRegion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchFirestoreRates(ctx)
	r, ok := rates["us-central1"]
	if !ok {
		t.Fatal("expected us-central1 key in fetched rates")
	}

	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"StorageRate", r.StorageRate, 0.15},
		{"StorageFreeGBMonth", r.StorageFreeGBMonth, 1.0},
		{"ReadRate", r.ReadRate, 0.03 / 100000},
		{"ReadFreeOpsMonth", r.ReadFreeOpsMonth, 1500000},
		{"WriteRate", r.WriteRate, 0.09 / 100000},
		{"WriteFreeOpsMonth", r.WriteFreeOpsMonth, 600000},
		{"DeleteRate", r.DeleteRate, 0.01 / 100000},
		{"DeleteFreeOpsMonth", r.DeleteFreeOpsMonth, 600000},
		{"SmallOpsRate", r.SmallOpsRate, 0},
		{"TTLDeleteRate", r.TTLDeleteRate, 0.01 / 100000},
		{"PITRStorageRate", r.PITRStorageRate, 0.15},
		{"ZonalBackupRate", r.ZonalBackupRate, 0.03},
		{"RestoreRate", r.RestoreRate, 0.20},
		{"CloneRate", r.CloneRate, 0.20},
	}
	for _, c := range cases {
		if abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %.9f, want %.9f", c.name, c.got, c.want)
		}
	}
}

// TestFetchFirestoreRates_MultiRegionsDoNotCollide verifies nam5 and nam7 are
// keyed distinctly even though their constituent geoTaxonomy.regions overlap
// on us-central1/us-central2.
func TestFetchFirestoreRates_MultiRegionsDoNotCollide(t *testing.T) {
	skus := []map[string]any{
		firestoreSKUFlat("Cloud Firestore Storage (nam5)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"us-central1", "us-central2", "us-east1"}, "0", 180000000), // $0.18/GiB-mo
		firestoreSKUFlat("Cloud Firestore Storage (nam7)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"us-central1", "us-central2", "us-east4"}, "0", 180000000), // same rate, different multi-region
		firestoreSKUFlat("Cloud Firestore Storage (eur3)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"europe-west1", "europe-west4", "europe-north1"}, "0", 180000000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchFirestoreRates(ctx)
	for _, key := range []string{"nam5", "nam7", "eur3"} {
		r, ok := rates[key]
		if !ok {
			t.Errorf("expected %q key in fetched rates, keys present: %v", key, mapKeys(rates))
			continue
		}
		if abs(r.StorageRate-0.18) > 1e-9 {
			t.Errorf("%s StorageRate = %.6f, want 0.180000", key, r.StorageRate)
		}
	}
	// The overlapping constituent regions themselves must NOT appear as keys
	// (they are not genuine regional keys here; only the multi-region short
	// names are).
	for _, key := range []string{"us-central1", "us-central2", "us-east1", "us-east4"} {
		if _, ok := rates[key]; ok {
			t.Errorf("unexpected regional key %q present; multi-region SKUs must key by short name only", key)
		}
	}
}

// TestFetchFirestoreRates_DistinguishesTwoRegions verifies two different
// REGIONAL SKUs produce two independent map entries with different rates —
// proving Firestore really is priced per-region, not globally.
func TestFetchFirestoreRates_DistinguishesTwoRegions(t *testing.T) {
	skus := []map[string]any{
		firestoreSKUFlat("Cloud Firestore Storage", "FirestoreStorage", "REGIONAL", []string{"us-east4"}, "0", 99000000),  // $0.099/GiB-mo (cheapest tier)
		firestoreSKUFlat("Cloud Firestore Storage", "FirestoreStorage", "REGIONAL", []string{"us-east1"}, "0", 180000000), // $0.18/GiB-mo (most expensive tier)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchFirestoreRates(ctx)
	if abs(rates["us-east4"].StorageRate-0.099) > 1e-9 {
		t.Errorf("us-east4 StorageRate = %.6f, want 0.099000", rates["us-east4"].StorageRate)
	}
	if abs(rates["us-east1"].StorageRate-0.18) > 1e-9 {
		t.Errorf("us-east1 StorageRate = %.6f, want 0.180000", rates["us-east1"].StorageRate)
	}
}

// TestFetchFirestoreRates_ExcludesGlobalCUDMetadataSKU verifies a GLOBAL SKU
// under the (otherwise in-scope) FirestoreReadOps resourceGroup is skipped
// and never overwrites a genuine regional read rate.
func TestFetchFirestoreRates_ExcludesGlobalCUDMetadataSKU(t *testing.T) {
	skus := []map[string]any{
		firestoreSKUFlat("Cloud Firestore Entity Read Operations CUD Metadata", "FirestoreReadOps", "GLOBAL", nil, "999", 0),
		firestoreSKUFlat("Cloud Firestore Entity Read Operations", "FirestoreReadOps", "REGIONAL", []string{"us-central1"}, "0", 300),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchFirestoreRates(ctx)
	if _, ok := rates["global"]; ok {
		t.Error("unexpected \"global\" key present; the GLOBAL CUD metadata SKU must be excluded entirely")
	}
	want := 0.03 / 100000
	if abs(rates["us-central1"].ReadRate-want) > 1e-9 {
		t.Errorf("us-central1 ReadRate = %.9f, want %.9f (GLOBAL SKU must not overwrite the real regional rate)", rates["us-central1"].ReadRate, want)
	}
}

// TestFetchFirestoreRates_ExcludesEnterpriseAndKeepsClone verifies an
// Enterprise-edition DatastoreOps SKU is excluded while a Standard-edition
// "clone" DatastoreOps SKU (same resourceGroup) is kept.
func TestFetchFirestoreRates_ExcludesEnterpriseAndKeepsClone(t *testing.T) {
	skus := []map[string]any{
		firestoreSKUFlat("Cloud Firestore Enterprise Edition Write Operations", "DatastoreOps", "REGIONAL", []string{"us-central1"}, "999", 0),
		firestoreSKUFlat("Cloud Firestore Database Clone Operation", "DatastoreOps", "REGIONAL", []string{"us-central1"}, "0", 200000000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchFirestoreRates(ctx)
	if abs(rates["us-central1"].CloneRate-0.20) > 1e-9 {
		t.Errorf("CloneRate = %.6f, want 0.200000 (Standard-edition clone kept)", rates["us-central1"].CloneRate)
	}
}

// TestFetchFirestoreRates_Cached verifies fetchFirestoreRates reads its
// derived rate map from cache instead of calling fetchSKUs/HTTP at all.
func TestFetchFirestoreRates_Cached(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchFirestoreRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := map[string]firestoreRates{
		"us-central1": {StorageRate: 0.999, ReadRate: 0.888},
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(firestoreRatesCacheKey, raw, p.cfg.MetadataTTL())

	rates := p.fetchFirestoreRates(ctx)
	if rates["us-central1"] != seeded["us-central1"] {
		t.Errorf("fetchFirestoreRates = %+v, want cached %+v", rates["us-central1"], seeded["us-central1"])
	}
}

// TestFetchFirestoreRates_FreeVariantAlwaysWinsRegardlessOfOrder verifies the
// "(with free tier)" variant wins over the plain variant for the same
// (region, bucket) regardless of which is scanned first.
func TestFetchFirestoreRates_FreeVariantAlwaysWinsRegardlessOfOrder(t *testing.T) {
	t.Run("plain_then_free", func(t *testing.T) {
		skus := []map[string]any{
			firestoreSKUFlat("Cloud Firestore Storage", "FirestoreStorage", "REGIONAL", []string{"us-central1"}, "0", 990000000),                           // plain, wrong rate
			firestoreSKUFreeTier("Cloud Firestore Storage (with free tier)", "FirestoreStorage", "REGIONAL", []string{"us-central1"}, 1.0, "0", 150000000), // free variant, correct rate
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(firestoreSKUResponse(skus))
		}))
		defer ts.Close()
		p := newTestProvider(t, ts)
		rates := p.fetchFirestoreRates(context.Background())
		if abs(rates["us-central1"].StorageRate-0.15) > 1e-9 {
			t.Errorf("StorageRate = %.6f, want 0.150000 (free-tier variant must win even though scanned second)", rates["us-central1"].StorageRate)
		}
	})
	t.Run("free_then_plain", func(t *testing.T) {
		skus := []map[string]any{
			firestoreSKUFreeTier("Cloud Firestore Storage (with free tier)", "FirestoreStorage", "REGIONAL", []string{"us-central1"}, 1.0, "0", 150000000), // free variant, correct rate
			firestoreSKUFlat("Cloud Firestore Storage", "FirestoreStorage", "REGIONAL", []string{"us-central1"}, "0", 990000000),                           // plain, wrong rate, scanned second
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(firestoreSKUResponse(skus))
		}))
		defer ts.Close()
		p := newTestProvider(t, ts)
		rates := p.fetchFirestoreRates(context.Background())
		if abs(rates["us-central1"].StorageRate-0.15) > 1e-9 {
			t.Errorf("StorageRate = %.6f, want 0.150000 (plain variant scanned second must not overwrite the free-tier variant)", rates["us-central1"].StorageRate)
		}
	})
}

// TestFetchFirestoreRates_SmallOpsReachesSwitchViaParsing proves a matched
// small_ops SKU actually reaches the switch in fetchFirestoreRates and sets
// r.SmallOpsRate from parsing — not merely by zero-value coincidence. Real
// Cloud Firestore small_ops SKUs are always $0 (verified live), which makes
// "parsed but zero" and "never reached the switch, so left at the region
// entry's zero-value default" indistinguishable by value alone. This fixture
// deliberately uses an atypical nonzero rate (real Firestore never does
// this) so a nonzero SmallOpsRate can ONLY come from the parsing path
// actually running, not from the zero-value default — guarding against the
// dead-code bug where the early `rate == 0 && freeThreshold == 0` guard fired
// for small_ops before the switch could set r.SmallOpsRate (issue #80).
func TestFetchFirestoreRates_SmallOpsReachesSwitchViaParsing(t *testing.T) {
	sku := firestoreSKUFlat("Cloud Firestore Small Operations", "FirestoreSmallOps", "REGIONAL", []string{"us-central1"}, "0", 123)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse([]map[string]any{sku}))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	rates := p.fetchFirestoreRates(context.Background())
	r, ok := rates["us-central1"]
	if !ok {
		t.Fatal("expected us-central1 key in fetched rates; a matched small_ops SKU must create the region entry")
	}
	if abs(r.SmallOpsRate-0.000000123) > 1e-12 {
		t.Errorf("SmallOpsRate = %.9f, want 0.000000123 (parsed from the live SKU, not left at the zero-value default)", r.SmallOpsRate)
	}
}

// --------------------------------------------------------------------------
// resolveFirestoreRates
// --------------------------------------------------------------------------

func TestResolveFirestoreRates_LiveRegionPresent(t *testing.T) {
	live := map[string]firestoreRates{
		"us-east4": {StorageRate: 0.099, StorageFreeGBMonth: 1.0, ReadRate: 0.03 / 100000, ReadFreeOpsMonth: 1500000,
			WriteRate: 0.09 / 100000, WriteFreeOpsMonth: 600000, DeleteRate: 0.01 / 100000, DeleteFreeOpsMonth: 600000,
			TTLDeleteRate: 0.01 / 100000, PITRStorageRate: 0.099, ZonalBackupRate: 0.0198, RestoreRate: 0.20, CloneRate: 0.20},
	}
	rates, usedFallback := resolveFirestoreRates(live, "us-east4")
	if usedFallback {
		t.Error("usedFallback = true, want false (region fully present in live map)")
	}
	if abs(rates.StorageRate-0.099) > 1e-9 {
		t.Errorf("StorageRate = %.6f, want 0.099000", rates.StorageRate)
	}
}

func TestResolveFirestoreRates_UnrecognizedRegionFallsBackEntirely(t *testing.T) {
	live := map[string]firestoreRates{
		"us-central1": {StorageRate: 0.15},
	}
	rates, usedFallback := resolveFirestoreRates(live, "mars-north1")
	if !usedFallback {
		t.Error("usedFallback = false, want true (region absent from live map)")
	}
	if rates != firestoreFallbackRates {
		t.Errorf("rates = %+v, want firestoreFallbackRates %+v", rates, firestoreFallbackRates)
	}
}

func TestResolveFirestoreRates_SmallOpsExcludedFromFallbackFlag(t *testing.T) {
	// Every field present and non-zero EXCEPT SmallOpsRate (which is
	// genuinely always 0 live) must not, by itself, trip usedFallback.
	live := map[string]firestoreRates{
		"us-central1": {
			StorageRate: 0.15, StorageFreeGBMonth: 1.0,
			ReadRate: 0.03 / 100000, ReadFreeOpsMonth: 1500000,
			WriteRate: 0.09 / 100000, WriteFreeOpsMonth: 600000,
			DeleteRate: 0.01 / 100000, DeleteFreeOpsMonth: 600000,
			TTLDeleteRate: 0.01 / 100000, PITRStorageRate: 0.15, ZonalBackupRate: 0.03,
			RestoreRate: 0.20, CloneRate: 0.20, SmallOpsRate: 0,
		},
	}
	_, usedFallback := resolveFirestoreRates(live, "us-central1")
	if usedFallback {
		t.Error("usedFallback = true, want false (SmallOpsRate==0 is genuinely live, not missing)")
	}
}

// TestResolveFirestoreRates_GenuineZeroFreeThresholdNotOverwritten proves the
// pickRateFreeThreshold carve-out: a bucket that IS matched live (nonzero
// Rate) but happens to have a genuine zero free threshold must keep that
// zero, not have it silently replaced by the fallback's nonzero free
// threshold. Before this fix, pickRate's "0 means missing" heuristic applied
// directly to *FreeGBMonth/*FreeOpsMonth fields would incorrectly treat this
// genuine zero as "missing" and substitute the fallback value instead — and
// would also incorrectly flag usedFallback for a fully-live-matched bucket.
func TestResolveFirestoreRates_GenuineZeroFreeThresholdNotOverwritten(t *testing.T) {
	// Every field present and non-zero EXCEPT ReadFreeOpsMonth (deliberately
	// 0, modeling "read bucket matched live but its free-tier SKU wasn't
	// found") and SmallOpsRate (genuinely always 0 live) — neither should,
	// by itself, trip usedFallback.
	live := map[string]firestoreRates{
		"us-central1": {
			StorageRate: 0.15, StorageFreeGBMonth: 1.0,
			// Read bucket matched live (nonzero ReadRate) but its free-tier
			// SKU wasn't found/matched, so ReadFreeOpsMonth is genuinely 0 —
			// distinct from "read bucket unmatched entirely" (ReadRate == 0).
			ReadRate: 0.03 / 100000, ReadFreeOpsMonth: 0,
			WriteRate: 0.09 / 100000, WriteFreeOpsMonth: 600000,
			DeleteRate: 0.01 / 100000, DeleteFreeOpsMonth: 600000,
			TTLDeleteRate: 0.01 / 100000, PITRStorageRate: 0.15, ZonalBackupRate: 0.03,
			RestoreRate: 0.20, CloneRate: 0.20, SmallOpsRate: 0,
		},
	}
	rates, usedFallback := resolveFirestoreRates(live, "us-central1")
	if usedFallback {
		t.Error("usedFallback = true, want false (ReadRate present live; ReadFreeOpsMonth==0 is genuine, not missing)")
	}
	if rates.ReadFreeOpsMonth != 0 {
		t.Errorf("ReadFreeOpsMonth = %v, want 0 (genuine live zero must not be replaced by the fallback's %v)", rates.ReadFreeOpsMonth, firestoreFallbackRates.ReadFreeOpsMonth)
	}
	if abs(rates.ReadRate-0.03/100000) > 1e-12 {
		t.Errorf("ReadRate = %v, want %v (live value)", rates.ReadRate, 0.03/100000)
	}
}

// --------------------------------------------------------------------------
// priceFirestore — dispatch, region handling, and cost math
// --------------------------------------------------------------------------

func TestPriceFirestore_ReturnsTenLineItems(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "us-central1"}}
	prices, _, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	if len(prices) != 10 {
		t.Fatalf("expected exactly 10 prices, got %d", len(prices))
	}
	wantDimensions := map[string]bool{
		"storage": true, "read": true, "write": true, "delete": true, "ttl_delete": true,
		"pitr_storage": true, "zonal_backup": true, "restore": true, "clone": true, "small_ops": true,
	}
	for _, pr := range prices {
		dim := pr.Attributes["dimension"]
		if !wantDimensions[dim] {
			t.Errorf("unexpected or missing dimension attribute: %q", dim)
		}
		delete(wantDimensions, dim)
		if pr.Region != "us-central1" {
			t.Errorf("dimension=%s: Region = %q, want %q (region-tagged, NOT global)", dim, pr.Region, "us-central1")
		}
		if pr.Attributes["scope"] == "global" {
			t.Errorf("dimension=%s: must not be stamped scope=global (Firestore is genuinely per-region)", dim)
		}
	}
	if len(wantDimensions) != 0 {
		t.Errorf("missing dimensions: %v", wantDimensions)
	}
}

func TestPriceFirestore_DefaultRegionIsUSCentral1(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{}
	prices, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	if breakdown["region"] != "us-central1" {
		t.Errorf("breakdown[region] = %v, want %q", breakdown["region"], "us-central1")
	}
	for _, pr := range prices {
		if pr.Region != "us-central1" {
			t.Errorf("Region = %q, want default us-central1", pr.Region)
		}
	}
}

// TestPriceFirestore_RegionsHaveDistinctRates verifies two different regions
// resolve to two different storage rates — proving region really selects the
// rate here, unlike DNS/KMS/Pub/Sub.
func TestPriceFirestore_RegionsHaveDistinctRates(t *testing.T) {
	skus := append(fakeFirestoreSKUsForRegion("us-central1"), fakeFirestoreSKUsForRegion("us-east4")...)
	// Override us-east4's storage SKU to the real cheapest-tier rate.
	for i, sku := range skus {
		if sku["description"] == "Cloud Firestore Storage (with free tier)" {
			geo, _ := sku["geoTaxonomy"].(map[string]any)
			regions, _ := geo["regions"].([]any)
			if len(regions) == 1 && regions[0] == "us-east4" {
				skus[i] = firestoreSKUFreeTier("Cloud Firestore Storage (with free tier)", "FirestoreStorage", "REGIONAL", []string{"us-east4"}, 1.0, "0", 99000000)
			}
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	specCentral := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "us-central1"}}
	pricesCentral, _, err := p.priceFirestore(ctx, specCentral)
	if err != nil {
		t.Fatalf("priceFirestore(us-central1): %v", err)
	}
	specEast4 := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "us-east4"}}
	pricesEast4, _, err := p.priceFirestore(ctx, specEast4)
	if err != nil {
		t.Fatalf("priceFirestore(us-east4): %v", err)
	}

	storageRate := func(prices []models.NormalizedPrice) float64 {
		for _, pr := range prices {
			if pr.Attributes["dimension"] == "storage" {
				return pr.PricePerUnit
			}
		}
		return -1
	}
	central := storageRate(pricesCentral)
	east4 := storageRate(pricesEast4)
	if abs(central-0.15) > 1e-9 {
		t.Errorf("us-central1 storage rate = %.6f, want 0.150000", central)
	}
	if abs(east4-0.099) > 1e-9 {
		t.Errorf("us-east4 storage rate = %.6f, want 0.099000", east4)
	}
	if abs(central-east4) < 1e-9 {
		t.Error("us-central1 and us-east4 storage rates must differ (Firestore is genuinely per-region)")
	}
}

// TestPriceFirestore_MultiRegionsDoNotCollide verifies nam5 and nam7 each
// resolve their own rates through the full priceFirestore path.
func TestPriceFirestore_MultiRegionsDoNotCollide(t *testing.T) {
	skus := []map[string]any{
		firestoreSKUFlat("Cloud Firestore Storage (nam5)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"us-central1", "us-central2", "us-east1"}, "0", 180000000),
		firestoreSKUFlat("Cloud Firestore Storage (nam7)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"us-central1", "us-central2", "us-east4"}, "0", 180000000),
		firestoreSKUFlat("Cloud Firestore Storage (eur3)", "FirestoreStorage", "MULTI_REGIONAL",
			[]string{"europe-west1", "europe-west4", "europe-north1"}, "0", 180000000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	for _, mr := range []string{"nam5", "nam7", "eur3"} {
		spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: mr}}
		prices, breakdown, err := p.priceFirestore(ctx, spec)
		if err != nil {
			t.Fatalf("priceFirestore(%s): %v", mr, err)
		}
		if fb, ok := breakdown["region_unrecognized"]; ok && fb == true {
			t.Errorf("%s: unexpectedly flagged region_unrecognized", mr)
		}
		for _, pr := range prices {
			if pr.Attributes["dimension"] == "storage" && abs(pr.PricePerUnit-0.18) > 1e-9 {
				t.Errorf("%s: storage rate = %.6f, want 0.180000", mr, pr.PricePerUnit)
			}
		}
	}
}

// TestPriceFirestore_UnrecognizedRegionFallsBackAndFlags verifies an
// unrecognized region falls back to us-central1 rates and is flagged
// distinctly from an ordinary live-fetch-failure fallback.
func TestPriceFirestore_UnrecognizedRegionFallsBackAndFlags(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "mars-north1"}}
	prices, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	if v, ok := breakdown["region_unrecognized"]; !ok || v != true {
		t.Errorf("breakdown[region_unrecognized] = %v, want true", v)
	}
	if _, ok := breakdown["region_unrecognized_note"]; !ok {
		t.Error("expected breakdown[region_unrecognized_note] to be present")
	}
	for _, pr := range prices {
		if pr.Attributes["dimension"] == "storage" && abs(pr.PricePerUnit-firestoreFallbackRates.StorageRate) > 1e-9 {
			t.Errorf("storage rate = %.6f, want fallback %.6f", pr.PricePerUnit, firestoreFallbackRates.StorageRate)
		}
	}
}

// TestPriceFirestore_RegionMatchIsCaseInsensitive verifies a differently
// cased region string ("US-CENTRAL1") still matches the live "us-central1"
// entry — proving region keys are normalized before both storing into and
// looking up from the live rates map (issue #80: previously an exact-case
// comparison meant any non-lowercase caller input would silently mismatch
// and fall back, even for a genuinely live-covered region).
func TestPriceFirestore_RegionMatchIsCaseInsensitive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "US-CENTRAL1"}}
	prices, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	if v, ok := breakdown["region_unrecognized"]; ok {
		t.Errorf("breakdown[region_unrecognized] = %v, want absent (differently-cased region must still match live us-central1)", v)
	}
	if v, ok := breakdown["fallback"]; ok && v == true {
		t.Errorf("breakdown[fallback] = %v, want absent/false (region should resolve from live data, not fallback)", v)
	}
	for _, pr := range prices {
		if pr.Attributes["dimension"] == "storage" && abs(pr.PricePerUnit-0.15) > 1e-9 {
			t.Errorf("storage rate = %.6f, want live 0.150000 (not fallback)", pr.PricePerUnit)
		}
	}
}

// --------------------------------------------------------------------------
// priceFirestore — free-tier vs no-free-tier cost math
// --------------------------------------------------------------------------

func TestPriceFirestore_StorageCostMath_BelowFreeTier(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 0.01 // well within the 1.0 GiBy.mo free allowance
	spec := &models.FirestorePricingSpec{StorageGB: &qty}
	_, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	got := mustFloat64(t, breakdown["storage_monthly_cost"], "storage_monthly_cost")
	if abs(got-0) > 1e-9 {
		t.Errorf("storage_monthly_cost = %.6f, want 0.000000 (within free tier)", got)
	}
}

func TestPriceFirestore_StorageCostMath_CrossesFreeTier(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	qty := 11.0 // 1.0 free + 10 paid GiB-months
	spec := &models.FirestorePricingSpec{StorageGB: &qty}
	_, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	got := mustFloat64(t, breakdown["storage_monthly_cost"], "storage_monthly_cost")
	want := 10.0 * 0.15
	if abs(got-want) > 1e-6 {
		t.Errorf("storage_monthly_cost = %.6f, want %.6f (tiered split, not flat rate*qty)", got, want)
	}
}

func TestPriceFirestore_ReadWriteDeleteFreeTiers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	cases := []struct {
		name          string
		buildSpec     func(qty *float64) *models.FirestorePricingSpec
		freeThreshold float64
		rate          float64
		breakdownKey  string
	}{
		{"read", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{ReadsPerMonth: q} }, 1500000, 0.03 / 100000, "read_monthly_cost"},
		{"write", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{WritesPerMonth: q} }, 600000, 0.09 / 100000, "write_monthly_cost"},
		{"delete", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{DeletesPerMonth: q} }, 600000, 0.01 / 100000, "delete_monthly_cost"},
	}
	for _, c := range cases {
		t.Run(c.name+"_below_free_tier", func(t *testing.T) {
			qty := c.freeThreshold - 1
			_, breakdown, err := p.priceFirestore(ctx, c.buildSpec(&qty))
			if err != nil {
				t.Fatalf("priceFirestore: %v", err)
			}
			got := mustFloat64(t, breakdown[c.breakdownKey], c.breakdownKey)
			if abs(got-0) > 1e-9 {
				t.Errorf("%s = %.6f, want 0.000000 (within free tier)", c.breakdownKey, got)
			}
		})
		t.Run(c.name+"_crosses_free_tier", func(t *testing.T) {
			qty := c.freeThreshold + 100000
			_, breakdown, err := p.priceFirestore(ctx, c.buildSpec(&qty))
			if err != nil {
				t.Fatalf("priceFirestore: %v", err)
			}
			got := mustFloat64(t, breakdown[c.breakdownKey], c.breakdownKey)
			want := 100000 * c.rate
			if abs(got-want) > 1e-9 {
				t.Errorf("%s = %.9f, want %.9f (only the excess over the free tier is billed)", c.breakdownKey, got, want)
			}
		})
	}
}

// TestPriceFirestore_NoFreeTierCategoriesAreAlwaysFlat proves TTL deletes,
// PITR storage, zonal backup storage, restore, and clone have NO effective
// free tier: cost = rate*qty even for tiny quantities, unlike storage/read/
// write/delete above.
func TestPriceFirestore_NoFreeTierCategoriesAreAlwaysFlat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	cases := []struct {
		name         string
		buildSpec    func(qty *float64) *models.FirestorePricingSpec
		rate         float64
		breakdownKey string
	}{
		{"ttl_delete", func(q *float64) *models.FirestorePricingSpec {
			return &models.FirestorePricingSpec{TTLDeletesPerMonth: q}
		}, 0.01 / 100000, "ttl_delete_monthly_cost"},
		{"pitr_storage", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{PITRStorageGB: q} }, 0.15, "pitr_storage_monthly_cost"},
		{"zonal_backup", func(q *float64) *models.FirestorePricingSpec {
			return &models.FirestorePricingSpec{ZonalBackupStorageGB: q}
		}, 0.03, "zonal_backup_monthly_cost"},
		{"restore", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{RestoreGB: q} }, 0.20, "restore_monthly_cost"},
		{"clone", func(q *float64) *models.FirestorePricingSpec { return &models.FirestorePricingSpec{CloneGB: q} }, 0.20, "clone_monthly_cost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			qty := 0.000001 // tiny quantity — a genuine free tier would zero this out
			_, breakdown, err := p.priceFirestore(ctx, c.buildSpec(&qty))
			if err != nil {
				t.Fatalf("priceFirestore: %v", err)
			}
			got := mustFloat64(t, breakdown[c.breakdownKey], c.breakdownKey)
			want := qty * c.rate
			if abs(got-want) > 1e-12 {
				t.Errorf("%s = %.15f, want %.15f (no free tier: even a tiny quantity is billed in full)", c.breakdownKey, got, want)
			}
			if got == 0 {
				t.Errorf("%s = 0, want > 0 (a genuine free tier would incorrectly zero this out)", c.breakdownKey)
			}
		})
	}
}

// TestPriceFirestore_SmallOpsAlwaysZero verifies the small_ops line item's
// rate is always $0, from both live and fallback data.
func TestPriceFirestore_SmallOpsAlwaysZero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "us-central1"}}
	prices, _, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	for _, pr := range prices {
		if pr.Attributes["dimension"] == "small_ops" && pr.PricePerUnit != 0 {
			t.Errorf("small_ops PricePerUnit = %v, want 0", pr.PricePerUnit)
		}
	}
}

// TestPriceFirestore_MonthlyCostSumsAllDimensions verifies the top-level
// monthly_cost sums every dimension supplied.
func TestPriceFirestore_MonthlyCostSumsAllDimensions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	storageQty := 11.0
	restoreQty := 5.0
	spec := &models.FirestorePricingSpec{StorageGB: &storageQty, RestoreGB: &restoreQty}
	_, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	storageCost := mustFloat64(t, breakdown["storage_monthly_cost"], "storage_monthly_cost")
	restoreCost := mustFloat64(t, breakdown["restore_monthly_cost"], "restore_monthly_cost")
	got := mustFloat64(t, breakdown["monthly_cost"], "monthly_cost")
	want := storageCost + restoreCost
	if abs(got-want) > 1e-6 {
		t.Errorf("monthly_cost = %.6f, want %.6f (sum of both dimensions)", got, want)
	}
}

func TestPriceFirestore_NoMonthlyCostWithoutQuantities(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{}
	_, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore: %v", err)
	}
	if _, ok := breakdown["monthly_cost"]; ok {
		t.Error("expected no monthly_cost key when no quantities are supplied")
	}
}

// --------------------------------------------------------------------------
// Fallback (entire catalog fetch failure)
// --------------------------------------------------------------------------

func TestPriceFirestore_FullFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.FirestorePricingSpec{BasePricingSpec: models.BasePricingSpec{Region: "us-central1"}}
	prices, breakdown, err := p.priceFirestore(ctx, spec)
	if err != nil {
		t.Fatalf("priceFirestore (fallback): %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true, got %v (ok=%v)", fb, ok)
	}
	// region_unrecognized must NOT be set here: the live catalog fetch
	// returned zero SKUs for EVERY region (a fetch failure), not just
	// "us-central1" specifically — the two failure modes must stay
	// distinguishable (see priceFirestore's regionUnrecognized comment).
	if v, ok := breakdown["region_unrecognized"]; ok {
		t.Errorf("breakdown[region_unrecognized] = %v, want absent (live fetch failed entirely for ALL regions, not just this one)", v)
	}
	for _, pr := range prices {
		if pr.Attributes["dimension"] == "storage" && abs(pr.PricePerUnit-firestoreFallbackRates.StorageRate) > 1e-9 {
			t.Errorf("fallback storage rate = %.6f, want %.6f", pr.PricePerUnit, firestoreFallbackRates.StorageRate)
		}
	}
}

// --------------------------------------------------------------------------
// Dispatch via Supports / GetPrice
// --------------------------------------------------------------------------

func TestPriceFirestore_DispatchViaGetPrice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(firestoreSKUResponse(fakeFirestoreSKUsForRegion("us-central1")))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	if !p.Supports(models.PricingDomainNoSQL, "firestore") {
		t.Fatal("Supports(nosql, firestore) = false, want true")
	}

	spec := &models.FirestorePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Domain:  models.PricingDomainNoSQL,
			Service: "firestore",
			Region:  "us-central1",
		},
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if len(result.PublicPrices) != 10 {
		t.Fatalf("GetPrice did not dispatch to priceFirestore: got %d prices", len(result.PublicPrices))
	}
	for _, pr := range result.PublicPrices {
		if pr.Service != "firestore" {
			t.Errorf("Service = %q, want %q", pr.Service, "firestore")
		}
	}
}

// mapKeys is a small test helper for readable failure messages.
func mapKeys(m map[string]firestoreRates) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
