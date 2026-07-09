// azure_sku_lookup_test.go tests LookupSKUAcrossRegionsGeneric (RC3-015
// Azure), the Azure raw-meterId counterpart to AWS's/GCP's get_price_by_sku
// lookup. Coverage focuses on the 7-step disambiguation algorithm documented
// at the top of azure_sku_lookup.go: primary/non-primary meter-region
// dedup, the spot/type/default hint branches, fails-closed no-match
// handling, and — critically — the tier-collision guard that must report
// Ambiguous instead of silently picking a wrong-priced row when multiple
// rows share every disambiguating field except SkuName/ProductName.
//
// Internal (package azure, not azure_test) so fixtures can be built directly
// as azureRetailItem values instead of via untyped JSON maps — mirrors
// aws_sku_lookup_test.go (package aws) / gcp_sku_lookup_test.go (package
// gcp)'s precedent for this kind of provider-internal algorithm test.
package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
)

// --------------------------------------------------------------------------
// Fixture helpers
// --------------------------------------------------------------------------

// azureSKUServer builds a fake Azure Retail Prices API server that always
// serves items as a single-page response (NextPageLink empty), regardless
// of the request's own $filter — every test here drives exactly one meterId
// per server, so no request-side routing is needed.
func azureSKUServer(t *testing.T, items []azureRetailItem) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(azureRetailResponse{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
}

// newSKULookupTestProvider creates a Provider backed by a temp-dir cache,
// pointing at srv. Every test must use a meterId unique to that test (not
// reused across tests) since azureSKUCatalogCache is a package-level cache
// keyed only by meterId — reusing a meterId across tests sharing the test
// binary's process lifetime would return a stale cached result from a
// different (by-then-closed) server instead of exercising the new one.
func newSKULookupTestProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(cm, 24*time.Hour, 7*24*time.Hour)
	p.SetBaseURL(srv.URL)
	p.SetHTTPClient(srv.Client())
	return p
}

func consumptionItem(meterID, region, skuName, productName string, tierMin, price float64) azureRetailItem {
	return azureRetailItem{
		RetailPrice:          price,
		SkuName:              skuName,
		ArmSkuName:           "Standard_Test",
		ProductName:          productName,
		MeterName:            skuName,
		ServiceName:          "Virtual Machines",
		ServiceFamily:        "Compute",
		MeterID:              meterID,
		ArmRegionName:        region,
		UnitOfMeasure:        "1 Hour",
		TierMinimumUnits:     tierMin,
		Type:                 "Consumption",
		IsPrimaryMeterRegion: true,
	}
}

// --------------------------------------------------------------------------
// (a) zero rows for a region -> NoMapping
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_NoRowsForRegion(t *testing.T) {
	items := []azureRetailItem{
		consumptionItem("meter-a", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192),
	}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-a", []string{"westeurope"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Regions) != 1 {
		t.Fatalf("expected 1 region result, got %d", len(result.Regions))
	}
	rr := result.Regions[0]
	if !rr.NoMapping {
		t.Errorf("expected NoMapping=true for region with zero rows, got %+v", rr)
	}
	if rr.Ambiguous {
		t.Errorf("expected Ambiguous=false, got true")
	}
}

// --------------------------------------------------------------------------
// (b) one row for a region -> unambiguous match
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_SingleRowMatch(t *testing.T) {
	items := []azureRetailItem{
		consumptionItem("meter-b", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192),
	}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-b", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if rr.NoMapping || rr.Ambiguous {
		t.Fatalf("expected a clean match, got %+v", rr)
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected exactly 1 price, got %d", len(rr.Prices))
	}
	if rr.Prices[0].PricePerUnit != 0.192 {
		t.Errorf("expected price 0.192, got %v", rr.Prices[0].PricePerUnit)
	}
	if rr.HintStatus != skulookup.HintStatusNoHint {
		t.Errorf("expected HintStatusNoHint, got %q", rr.HintStatus)
	}
}

// --------------------------------------------------------------------------
// (c) Consumption vs DevTestConsumption -> default picks Consumption,
//     hint picks DevTestConsumption
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_TypeDefaultAndHint(t *testing.T) {
	consumption := consumptionItem("meter-c", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192)
	devTest := consumptionItem("meter-c", "eastus", "D4s v3 DevTest", "Virtual Machines DSv3 Series", 0, 0.096)
	devTest.Type = "DevTestConsumption"

	items := []azureRetailItem{consumption, devTest}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	// No hint: default filters to Type=="Consumption".
	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-c", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected unambiguous default match, got ambiguous: %+v", rr)
	}
	if len(rr.Prices) != 1 || rr.Prices[0].PricePerUnit != 0.192 {
		t.Fatalf("expected the Consumption row (0.192), got %+v", rr.Prices)
	}

	// hint="DevTestConsumption": picks the other row.
	result, err = p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-c", []string{"eastus"}, "",
		skulookup.SKUHint{ProductFamilyHint: "DevTestConsumption"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr = result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected unambiguous hinted match, got ambiguous: %+v", rr)
	}
	if len(rr.Prices) != 1 || rr.Prices[0].PricePerUnit != 0.096 {
		t.Fatalf("expected the DevTestConsumption row (0.096), got %+v", rr.Prices)
	}
	if rr.HintStatus != skulookup.HintStatusResolved {
		t.Errorf("expected HintStatusResolved, got %q", rr.HintStatus)
	}
}

// --------------------------------------------------------------------------
// (d) genuine tiered/graduated fixture -> lowest tier selected, not
//     ambiguous
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_GenuineTierLadder(t *testing.T) {
	items := []azureRetailItem{
		consumptionItem("meter-d", "eastus", "S1 Blob Storage", "Blob Storage", 0, 0.0184),
		consumptionItem("meter-d", "eastus", "S1 Blob Storage", "Blob Storage", 51200, 0.0177),
		consumptionItem("meter-d", "eastus", "S1 Blob Storage", "Blob Storage", 512000, 0.017),
	}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-d", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected a genuine tier ladder to resolve without ambiguity, got %+v", rr)
	}
	if !rr.Tiered {
		t.Errorf("expected Tiered=true")
	}
	if len(rr.Prices) != 3 {
		t.Fatalf("expected all 3 tiers surfaced, got %d", len(rr.Prices))
	}
	if rr.Prices[0].PricePerUnit != 0.0184 {
		t.Errorf("expected the base (lowest-tier) row first, got %+v", rr.Prices[0])
	}
	// Every tier row must carry tier_start_usage — bom.go's
	// gcpGraduatedTieredCost (shared, provider-agnostic) reads only this
	// attribute to bracket usage against each tier; without it, a tiered
	// Azure SKU silently prices at $0.00/mo (every tier gets skipped by
	// tierStartUsage's ok=false path). See azureSKUItemToPrice.
	wantStart := []string{"0", "51200", "512000"}
	for i, price := range rr.Prices {
		if got := price.Attributes["tier_start_usage"]; got != wantStart[i] {
			t.Errorf("tier %d: expected tier_start_usage=%q, got %q (attrs %+v)", i, wantStart[i], got, price.Attributes)
		}
	}
}

// --------------------------------------------------------------------------
// (d2) product-family collision under the DEFAULT (no-hint) path: two
//      distinct Consumption products sharing tierMinimumUnits=0 must NOT be
//      treated as a genuine tier ladder. This is the failure branch of
//      resolveAzureTierGroup (len(groups) != 1) — the direct sibling of the
//      Reservation-collision regression in scenario (g), but reached via the
//      default (no-hint) path instead of an explicit hint, so it exercises
//      the group-count guard itself rather than the explicit-hint bypass.
//      Per the T41 precedent (docs/plans/T41-sku-lookup.md), a "cheapest"
//      tiebreak here would silently return the wrong product at the wrong
//      price; both candidates must survive to the Ambiguous report instead.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_DistinctConsumptionProductsStayAmbiguous(t *testing.T) {
	items := []azureRetailItem{
		consumptionItem("meter-d2", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192),
		consumptionItem("meter-d2", "eastus", "E4s v3", "Virtual Machines ESv3 Series", 0, 0.252),
	}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-d2", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.Ambiguous {
		t.Fatalf("expected two distinct Consumption products with no hint to stay Ambiguous rather than be treated as a tier ladder, got %+v", rr)
	}
	if rr.HintStatus != skulookup.HintStatusAmbiguous {
		t.Errorf("expected HintStatusAmbiguous, got %q", rr.HintStatus)
	}
	if rr.Tiered {
		t.Errorf("expected Tiered=false — these are two different products, not tiers of one product")
	}
	if len(rr.Prices) != 2 {
		t.Fatalf("expected both candidates preserved (no arbitrary pick), got %d prices: %+v", len(rr.Prices), rr.Prices)
	}
}

// --------------------------------------------------------------------------
// (e) primary/non-primary duplicate-region fixture
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_PrimaryMeterRegionDedup(t *testing.T) {
	primary := consumptionItem("meter-e", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192)
	duplicate := consumptionItem("meter-e", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192)
	duplicate.IsPrimaryMeterRegion = false

	items := []azureRetailItem{primary, duplicate}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-e", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected the primary/non-primary dedup to resolve without ambiguity, got %+v", rr)
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected the duplicate row dropped, got %d prices", len(rr.Prices))
	}
	if rr.Prices[0].Attributes["isPrimaryMeterRegion"] != "true" {
		t.Errorf("expected the surviving row to be the primary one, got attrs %+v", rr.Prices[0].Attributes)
	}
}

// --------------------------------------------------------------------------
// (f) Spot fixture: product_family_hint="spot" resolves via meterName
//     substring
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_SpotHintMeterNameSubstring(t *testing.T) {
	onDemand := consumptionItem("meter-f", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192)
	spot := consumptionItem("meter-f", "eastus", "D4s v3 Spot", "Virtual Machines DSv3 Series", 0, 0.05)
	spot.MeterName = "D4s v3 Spot"

	items := []azureRetailItem{onDemand, spot}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-f", []string{"eastus"}, "",
		skulookup.SKUHint{ProductFamilyHint: "spot"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected the spot hint to resolve without ambiguity, got %+v", rr)
	}
	if len(rr.Prices) != 1 || rr.Prices[0].PricePerUnit != 0.05 {
		t.Fatalf("expected the Spot row (0.05), got %+v", rr.Prices)
	}
	if rr.HintStatus != skulookup.HintStatusResolved {
		t.Errorf("expected HintStatusResolved, got %q", rr.HintStatus)
	}
}

// --------------------------------------------------------------------------
// (g) THE CRITICAL NEW FIXTURE: Reservation-tier collision -> Ambiguous,
//     not an arbitrarily-picked price
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_ReservationTierCollisionStaysAmbiguous(t *testing.T) {
	rowA := azureRetailItem{
		RetailPrice:          5048.0,
		SkuName:              "D4s v3 Reserved",
		ArmSkuName:           "Standard_D4s_v3",
		ProductName:          "Virtual Machines DSv3 Series",
		MeterName:            "D4s v3",
		ServiceName:          "Virtual Machines",
		ServiceFamily:        "Compute",
		MeterID:              "meter-g",
		ArmRegionName:        "eastus",
		UnitOfMeasure:        "1 Year",
		TierMinimumUnits:     0.0,
		Type:                 "Reservation",
		IsPrimaryMeterRegion: true,
	}
	// A colliding row: same meterId/region/type/isPrimaryMeterRegion/
	// tierMinimumUnits, but a completely different product (different
	// SkuName/ProductName) and wildly different RetailPrice — the exact
	// live-data shape documented in azure_sku_lookup.go's step-7 doc.
	rowB := azureRetailItem{
		RetailPrice:          118201.0,
		SkuName:              "M128s Reserved",
		ArmSkuName:           "Standard_M128s",
		ProductName:          "Virtual Machines Msv2 Series",
		MeterName:            "M128s",
		ServiceName:          "Virtual Machines",
		ServiceFamily:        "Compute",
		MeterID:              "meter-g",
		ArmRegionName:        "eastus",
		UnitOfMeasure:        "3 Years",
		TierMinimumUnits:     0.0,
		Type:                 "Reservation",
		IsPrimaryMeterRegion: true,
	}

	items := []azureRetailItem{rowA, rowB}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-g", []string{"eastus"}, "",
		skulookup.SKUHint{ProductFamilyHint: "Reservation"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.Ambiguous {
		t.Fatalf("expected Ambiguous=true for the Reservation tier collision, got a resolved single price: %+v", rr)
	}
	if len(rr.Prices) != 2 {
		t.Fatalf("expected both colliding candidates preserved in Prices, got %d", len(rr.Prices))
	}
}

// --------------------------------------------------------------------------
// (g2) resolveAzureTierGroup, exercised DIRECTLY against Reservation-shaped
//      colliding rows. In production, resolveAzureTierGroup is only ever
//      called from the default (no-hint) path (resolveAzureSKURegion), and
//      the no-hint path's own applyAzureSKUHint pre-filters to
//      Type=="Consumption" before reaching it — so a Reservation row can
//      never structurally reach this function today, and (g)'s end-to-end
//      test above exercises the collision only via the coarser
//      explicit-hint-always-ambiguous rule (step 6), not this guard itself.
//      This test closes that direct-coverage gap: it proves the
//      grouping/monotonicity guard would ALSO correctly reject this exact
//      live-data collision shape (distinct SkuName/ProductName, identical
//      tierMinimumUnits==0.0, wildly different RetailPrice) if it were ever
//      reached, without changing resolveAzureSKURegion's call graph or
//      weakening the guard itself.
// --------------------------------------------------------------------------

func TestResolveAzureTierGroup_ReservationCollisionRejected(t *testing.T) {
	rowA := azureRetailItem{
		RetailPrice:      5048.0,
		SkuName:          "D4s v3 Reserved",
		ProductName:      "Virtual Machines DSv3 Series",
		TierMinimumUnits: 0.0,
		Type:             "Reservation",
	}
	rowB := azureRetailItem{
		RetailPrice:      118201.0,
		SkuName:          "M128s Reserved",
		ProductName:      "Virtual Machines Msv2 Series",
		TierMinimumUnits: 0.0,
		Type:             "Reservation",
	}

	tiers, ok := resolveAzureTierGroup([]azureRetailItem{rowA, rowB})
	if ok {
		t.Fatalf("expected resolveAzureTierGroup to reject a collision across distinct (SkuName, ProductName) groups, got ok=true tiers=%+v", tiers)
	}
	if tiers != nil {
		t.Errorf("expected nil tiers on rejection, got %+v", tiers)
	}
}

// TestResolveAzureTierGroup_DuplicateTierMinimumUnitsRejected covers the
// second independent guard inside resolveAzureTierGroup: even within a
// single (SkuName, ProductName) group, a duplicate TierMinimumUnits is not
// a real tier ladder and must not be resolved.
func TestResolveAzureTierGroup_DuplicateTierMinimumUnitsRejected(t *testing.T) {
	rowA := azureRetailItem{RetailPrice: 5048.0, SkuName: "D4s v3 Reserved", ProductName: "Virtual Machines DSv3 Series", TierMinimumUnits: 0.0, Type: "Reservation"}
	rowB := azureRetailItem{RetailPrice: 118201.0, SkuName: "D4s v3 Reserved", ProductName: "Virtual Machines DSv3 Series", TierMinimumUnits: 0.0, Type: "Reservation"}

	if tiers, ok := resolveAzureTierGroup([]azureRetailItem{rowA, rowB}); ok {
		t.Fatalf("expected rejection on duplicate TierMinimumUnits within one group, got ok=true tiers=%+v", tiers)
	}
}

// --------------------------------------------------------------------------
// (h) hint matches zero rows -> HintStatusNoMatch, fails closed
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_HintMatchesNothingFailsClosed(t *testing.T) {
	consumption := consumptionItem("meter-h", "eastus", "D4s v3", "Virtual Machines DSv3 Series", 0, 0.192)
	reservation := consumptionItem("meter-h", "eastus", "D4s v3 Reserved", "Virtual Machines DSv3 Series", 0, 1016.16)
	reservation.Type = "Reservation"

	items := []azureRetailItem{consumption, reservation}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-h", []string{"eastus"}, "",
		skulookup.SKUHint{ProductFamilyHint: "DevTestConsumption"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.Ambiguous {
		t.Fatalf("expected Ambiguous=true when the hint matches nothing, got %+v", rr)
	}
	if rr.HintStatus != skulookup.HintStatusNoMatch {
		t.Errorf("expected HintStatusNoMatch, got %q", rr.HintStatus)
	}
	if len(rr.Prices) != 2 {
		t.Fatalf("expected the original unfiltered candidate set (2 rows) preserved, got %d", len(rr.Prices))
	}
}

// --------------------------------------------------------------------------
// (h2) DEFAULT (no-hint) path's own Consumption filter matches zero rows ->
//      HintStatusNoHint, NOT HintStatusNoMatch. HintStatusNoMatch is
//      documented (skulookup.go) as meaning "a hint was SUPPLIED but
//      matched zero rows" — this scenario supplies no hint at all (every
//      row here is a Reservation row, so applyAzureSKUHint's own default
//      Type=="Consumption" filter is what empties the set, not a caller
//      hint), so mislabeling it NoMatch would misreport a hint that was
//      never given.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_NoHintDefaultFilterEmptyIsNoHintNotNoMatch(t *testing.T) {
	reservation1 := consumptionItem("meter-h2", "eastus", "D4s v3 Reserved 1yr", "Virtual Machines DSv3 Series", 0, 1016.16)
	reservation1.Type = "Reservation"
	reservation3 := consumptionItem("meter-h2", "eastus", "D4s v3 Reserved 3yr", "Virtual Machines DSv3 Series", 0, 2500.0)
	reservation3.Type = "Reservation"

	items := []azureRetailItem{reservation1, reservation3}
	srv := azureSKUServer(t, items)
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-h2", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.Ambiguous {
		t.Fatalf("expected Ambiguous=true when the default Consumption filter matches nothing, got %+v", rr)
	}
	if rr.HintStatus != skulookup.HintStatusNoHint {
		t.Errorf("expected HintStatusNoHint (no hint was supplied), got %q", rr.HintStatus)
	}
	if len(rr.Prices) != 2 {
		t.Fatalf("expected the original unfiltered candidate set (2 rows) preserved, got %d", len(rr.Prices))
	}
}

// --------------------------------------------------------------------------
// azureSKUUnit: best-effort UnitOfMeasure -> models.PriceUnit derivation.
// Regression coverage for the previous hardcoded-per-hour default, which
// overstated MonthlyCost() by ~730x for any non-hourly meter (see
// azureSKUUnit's doc comment).
// --------------------------------------------------------------------------

func TestAzureSKUUnit(t *testing.T) {
	cases := []struct {
		name string
		item azureRetailItem
		want models.PriceUnit
	}{
		{"hour", azureRetailItem{UnitOfMeasure: "1 Hour"}, models.PriceUnitPerHour},
		{"gb-month", azureRetailItem{UnitOfMeasure: "1 GB/Month"}, models.PriceUnitPerGBMonth},
		{"gb", azureRetailItem{UnitOfMeasure: "1 GB"}, models.PriceUnitPerGB},
		{"gb-second", azureRetailItem{UnitOfMeasure: "1 GB Second"}, models.PriceUnitPerGBSecond},
		{"month", azureRetailItem{UnitOfMeasure: "1/Month"}, models.PriceUnitPerMonth},
		{"request-by-meter", azureRetailItem{UnitOfMeasure: "10K", MeterName: "Standard Execution"}, models.PriceUnitPerRequest},
		{"operation-by-uom", azureRetailItem{UnitOfMeasure: "10K Operations"}, models.PriceUnitPerOperation},
		// Reservation rows: UnitOfMeasure is a contract-length label, not an
		// hourly rate — must NOT fall back to per_hour (that was the bug:
		// MonthlyCost() would multiply a total contract price by 730).
		{"reservation-1yr", azureRetailItem{UnitOfMeasure: "1 Year"}, models.PriceUnitPerUnit},
		{"reservation-3yr", azureRetailItem{UnitOfMeasure: "3 Years"}, models.PriceUnitPerUnit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := azureSKUUnit(tc.item); got != tc.want {
				t.Errorf("azureSKUUnit(%+v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// azureSKUItemTerm: PricingTerm derivation, including Fix #8's "Low
// Priority" spot-substring branch (the pre-fix version only matched
// "spot", so Azure Batch/AKS "Low Priority" meters were misclassified as
// PricingTermOnDemand).
// --------------------------------------------------------------------------

func TestAzureSKUItemTerm(t *testing.T) {
	cases := []struct {
		name string
		item azureRetailItem
		want models.PricingTerm
	}{
		{"spot-by-meter-name", azureRetailItem{MeterName: "D4s v3 Spot", Type: "Consumption"}, models.PricingTermSpot},
		{"low-priority-by-meter-name", azureRetailItem{MeterName: "D4s v3 Low Priority", Type: "Consumption"}, models.PricingTermSpot},
		{"reservation", azureRetailItem{MeterName: "D4s v3", Type: "Reservation"}, models.PricingTermReserved1Yr},
		{"on-demand-consumption", azureRetailItem{MeterName: "D4s v3", Type: "Consumption"}, models.PricingTermOnDemand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := azureSKUItemTerm(tc.item); got != tc.want {
				t.Errorf("azureSKUItemTerm(%+v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Extra: fetch-level error is not reported as NoMapping
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_FetchErrorNotNoMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := newSKULookupTestProvider(t, srv)

	result, err := p.LookupSKUAcrossRegionsGeneric(context.Background(), "meter-err", []string{"eastus"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	rr := result.Regions[0]
	if rr.NoMapping {
		t.Errorf("a fetch failure must not be reported as NoMapping (that asserts 'checked, not found')")
	}
	if rr.Error == "" {
		t.Errorf("expected a non-empty Error for a fetch failure")
	}
}
