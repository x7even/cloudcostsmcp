package aws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// ---------------------------------------------------------------------------
// SP test fixtures
// ---------------------------------------------------------------------------

// buildSPTestServer creates an httptest.Server that serves:
//   - GET /current/region_index.json  — returns a region index pointing to the per-region file
//   - GET /test-version/{region}/index.json  — returns the minimal SP JSON fixture
//
// The caller must set spIndexBaseURL = server.URL and must call server.Close().
func buildSPTestServer(t *testing.T, region string, fixture []byte) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/region_index.json"):
			idx := map[string]any{
				"regions": []map[string]string{
					{
						"regionCode": region,
						"versionUrl": srv.URL + "/test-version/" + region + "/index.json",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			if encErr := enc.Encode(idx); encErr != nil {
				t.Errorf("encode region index: %v", encErr)
			}

		case strings.HasSuffix(r.URL.Path, "/index.json"):
			w.Header().Set("Content-Type", "application/json")
			_, writeErr := w.Write(fixture)
			if writeErr != nil {
				t.Errorf("write fixture: %v", writeErr)
			}

		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// overrideSPBaseURL saves spIndexBaseURL, sets it to serverURL, and
// registers cleanup to restore the original.
func overrideSPBaseURL(t *testing.T, serverURL string) {
	t.Helper()
	orig := spIndexBaseURL
	spIndexBaseURL = serverURL
	t.Cleanup(func() { spIndexBaseURL = orig })
}

// minimalSPFixture returns a minimal SP bulk JSON with:
//   - 2 CSP products (1yr No Upfront, 3yr No Upfront)
//   - 2 ISP products (1yr No Upfront for m5 family, 1yr No Upfront for c5 family)
//   - Matching terms with synthetic rates
//
// Structure: {"products":[...],"terms":{"savingsPlan":[...]}}
// products is an ARRAY (not a map) — critical for the stream parser.
const minimalSPFixtureJSON = `{
  "products": [
    {
      "sku": "CSP1YR",
      "productFamily": "ComputeSavingsPlans",
      "attributes": {
        "purchaseOption": "No Upfront",
        "purchaseTerm": "1yr"
      }
    },
    {
      "sku": "CSP3YR",
      "productFamily": "ComputeSavingsPlans",
      "attributes": {
        "purchaseOption": "No Upfront",
        "purchaseTerm": "3yr"
      }
    },
    {
      "sku": "ISP1YR_M5",
      "productFamily": "EC2 Instance Savings Plans",
      "attributes": {
        "purchaseOption": "No Upfront",
        "purchaseTerm": "1yr",
        "instanceType": "m5"
      }
    },
    {
      "sku": "ISP1YR_C5",
      "productFamily": "EC2 Instance Savings Plans",
      "attributes": {
        "purchaseOption": "No Upfront",
        "purchaseTerm": "1yr",
        "instanceType": "c5"
      }
    }
  ],
  "terms": {
    "savingsPlan": [
      {
        "sku": "CSP1YR",
        "description": "Compute Savings Plan 1yr No Upfront",
        "effectiveDate": "2026-01-01T00:00:00Z",
        "leaseContractLength": {"duration": 1, "unit": "year"},
        "rates": [
          {
            "discountedSku": "EC2_M5_XLARGE",
            "discountedUsageType": "BoxUsage:m5.xlarge",
            "discountedOperation": "RunInstances",
            "discountedServiceCode": "AmazonEC2",
            "rateCode": "CSP1YR.EC2_M5_XLARGE",
            "unit": "Hrs",
            "discountedRate": {"price": "0.1410", "currency": "USD"},
            "discountedRegionCode": "us-east-1",
            "discountedInstanceType": ""
          },
          {
            "discountedSku": "EC2_M5_XLARGE_WIN",
            "discountedUsageType": "BoxUsage:m5.xlarge",
            "discountedOperation": "RunInstances:0010",
            "discountedServiceCode": "AmazonEC2",
            "rateCode": "CSP1YR.EC2_M5_XLARGE_WIN",
            "unit": "Hrs",
            "discountedRate": {"price": "0.2800", "currency": "USD"},
            "discountedRegionCode": "us-east-1",
            "discountedInstanceType": ""
          }
        ]
      },
      {
        "sku": "CSP3YR",
        "description": "Compute Savings Plan 3yr No Upfront",
        "effectiveDate": "2026-01-01T00:00:00Z",
        "leaseContractLength": {"duration": 3, "unit": "year"},
        "rates": [
          {
            "discountedSku": "EC2_M5_XLARGE",
            "discountedUsageType": "BoxUsage:m5.xlarge",
            "discountedOperation": "RunInstances",
            "discountedServiceCode": "AmazonEC2",
            "rateCode": "CSP3YR.EC2_M5_XLARGE",
            "unit": "Hrs",
            "discountedRate": {"price": "0.0970", "currency": "USD"},
            "discountedRegionCode": "us-east-1",
            "discountedInstanceType": ""
          }
        ]
      },
      {
        "sku": "ISP1YR_M5",
        "description": "EC2 Instance SP 1yr No Upfront m5",
        "effectiveDate": "2026-01-01T00:00:00Z",
        "leaseContractLength": {"duration": 1, "unit": "year"},
        "rates": [
          {
            "discountedSku": "EC2_M5_XLARGE",
            "discountedUsageType": "BoxUsage:m5.xlarge",
            "discountedOperation": "RunInstances",
            "discountedServiceCode": "AmazonEC2",
            "rateCode": "ISP1YR_M5.EC2_M5_XLARGE",
            "unit": "Hrs",
            "discountedRate": {"price": "0.1210", "currency": "USD"},
            "discountedRegionCode": "us-east-1",
            "discountedInstanceType": "m5.xlarge"
          }
        ]
      },
      {
        "sku": "ISP1YR_C5",
        "description": "EC2 Instance SP 1yr No Upfront c5",
        "effectiveDate": "2026-01-01T00:00:00Z",
        "leaseContractLength": {"duration": 1, "unit": "year"},
        "rates": [
          {
            "discountedSku": "EC2_C5_XLARGE",
            "discountedUsageType": "BoxUsage:c5.xlarge",
            "discountedOperation": "RunInstances",
            "discountedServiceCode": "AmazonEC2",
            "rateCode": "ISP1YR_C5.EC2_C5_XLARGE",
            "unit": "Hrs",
            "discountedRate": {"price": "0.1070", "currency": "USD"},
            "discountedRegionCode": "us-east-1",
            "discountedInstanceType": "c5.xlarge"
          }
        ]
      }
    ]
  }
}`

// ---------------------------------------------------------------------------
// Helper: build a provider + SP test server and wire them together
// ---------------------------------------------------------------------------

func setupSPTest(t *testing.T, region string) (*Provider, *httptest.Server) {
	t.Helper()
	p := newTestProvider(t)
	srv := buildSPTestServer(t, region, []byte(minimalSPFixtureJSON))
	overrideSPBaseURL(t, srv.URL)
	// Also override the bulk URL so GetComputePrice (called for OD rate) returns
	// empty quickly rather than hitting the real network.
	overrideBulkBaseURL(t, srv.URL)
	return p, srv
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestComputeSP_1yr_PriceReturned verifies that a CSP 1yr No Upfront lookup
// returns the correct rate from the fixture.
func TestComputeSP_1yr_PriceReturned(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermComputeSP,
		},
		ResourceType: "m5.xlarge",
		OS:           "Linux",
	}
	payOpt := "No Upfront"
	years := 1
	spec.PaymentOption = &payOpt
	spec.CommitmentYears = &years

	result, err := p.GetSavingsPlanPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetSavingsPlanPrice: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatalf("expected public prices, got empty slice (note=%q)", result.Note)
	}
	got := result.PublicPrices[0].PricePerUnit
	const want = 0.141
	if got != want {
		t.Errorf("CSP 1yr price = %v, want %v", got, want)
	}
	if result.PublicPrices[0].Unit != models.PriceUnitPerHour {
		t.Errorf("unit = %v, want per_hour", result.PublicPrices[0].Unit)
	}
}

// TestEC2ISP_1yr_PriceReturned verifies ISP 1yr rate for m5.xlarge.
func TestEC2ISP_1yr_PriceReturned(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermEC2InstanceSP,
		},
		ResourceType: "m5.xlarge",
		OS:           "Linux",
	}
	payOpt := "No Upfront"
	years := 1
	spec.PaymentOption = &payOpt
	spec.CommitmentYears = &years

	result, err := p.GetSavingsPlanPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetSavingsPlanPrice ISP: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatalf("expected public prices for ISP, got empty (note=%q)", result.Note)
	}
	got := result.PublicPrices[0].PricePerUnit
	const want = 0.121
	if got != want {
		t.Errorf("ISP 1yr m5.xlarge price = %v, want %v", got, want)
	}
}

// TestComputeSP_3yr_DiscountDeeper verifies that the 3yr CSP rate is lower
// than the 1yr rate (deeper discount for longer commitment).
func TestComputeSP_3yr_DiscountDeeper(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	spec1yr := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermComputeSP,
		},
		ResourceType: "m5.xlarge",
		OS:           "Linux",
	}
	payOpt := "No Upfront"
	y1 := 1
	y3 := 3
	spec1yr.PaymentOption = &payOpt
	spec1yr.CommitmentYears = &y1

	spec3yr := *spec1yr
	spec3yr.CommitmentYears = &y3

	r1, err := p.GetSavingsPlanPrice(context.Background(), spec1yr)
	if err != nil || len(r1.PublicPrices) == 0 {
		t.Fatalf("CSP 1yr: err=%v prices=%v", err, r1)
	}
	r3, err := p.GetSavingsPlanPrice(context.Background(), &spec3yr)
	if err != nil || len(r3.PublicPrices) == 0 {
		t.Fatalf("CSP 3yr: err=%v prices=%v", err, r3)
	}

	price1 := r1.PublicPrices[0].PricePerUnit
	price3 := r3.PublicPrices[0].PricePerUnit
	if price3 >= price1 {
		t.Errorf("3yr CSP rate (%v) should be lower than 1yr (%v)", price3, price1)
	}
}

// TestEDPAdjustment_ReducesRate verifies that edp_discount_pct correctly reduces
// the SP rate and populates ContractedPrices.
func TestEDPAdjustment_ReducesRate(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	edpPct := 0.10 // 10% EDP discount
	payOpt := "No Upfront"
	years := 1
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermComputeSP,
		},
		ResourceType:   "m5.xlarge",
		OS:             "Linux",
		PaymentOption:  &payOpt,
		CommitmentYears: &years,
		EDPDiscountPct:  &edpPct,
	}

	result, err := p.GetSavingsPlanPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetSavingsPlanPrice with EDP: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatalf("expected SP public price, got empty")
	}
	if len(result.ContractedPrices) == 0 {
		t.Fatalf("expected EDP contracted price, got empty")
	}

	spRate := result.PublicPrices[0].PricePerUnit   // 0.141
	edpRate := result.ContractedPrices[0].PricePerUnit // 0.141 * 0.9 = 0.1269

	const wantSP = 0.141
	wantEDP := wantSP * (1.0 - edpPct)

	if spRate != wantSP {
		t.Errorf("SP rate = %v, want %v", spRate, wantSP)
	}
	if edpRate != wantEDP {
		t.Errorf("EDP rate = %v, want %v", edpRate, wantEDP)
	}
	if result.ContractedPrices[0].Attributes["edp_source"] != "user_supplied" {
		t.Errorf("edp_source attribute missing or wrong: %v", result.ContractedPrices[0].Attributes)
	}
}

// TestIneligibleInstanceType_ReturnsEmpty verifies that a non-covered instance
// type returns an empty result with a Note rather than an error.
func TestIneligibleInstanceType_ReturnsEmpty(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	payOpt := "No Upfront"
	years := 1
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermEC2InstanceSP,
		},
		// p3.2xlarge is NOT in the fixture.
		ResourceType:    "p3.2xlarge",
		OS:              "Linux",
		PaymentOption:   &payOpt,
		CommitmentYears: &years,
	}

	result, err := p.GetSavingsPlanPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error for non-covered instance: %v", err)
	}
	if len(result.PublicPrices) != 0 {
		t.Errorf("expected empty public prices for non-covered instance, got %v", result.PublicPrices)
	}
	if result.Note == "" {
		t.Errorf("expected a Note explaining why no price was returned")
	}
}

// TestCatalogIncludesNewTerms verifies that DescribeCatalog lists both SP terms.
func TestCatalogIncludesNewTerms(t *testing.T) {
	p := newTestProvider(t)
	cat, err := p.DescribeCatalog(context.Background())
	if err != nil {
		t.Fatalf("DescribeCatalog: %v", err)
	}

	computeTerms, ok := cat.SupportedTerms["compute"]
	if !ok {
		t.Fatal("no 'compute' key in SupportedTerms")
	}

	termsSet := make(map[string]bool, len(computeTerms))
	for _, term := range computeTerms {
		termsSet[term] = true
	}

	for _, want := range []string{
		string(models.PricingTermComputeSP),
		string(models.PricingTermEC2InstanceSP),
		string(models.PricingTermSpot),
	} {
		if !termsSet[want] {
			t.Errorf("SupportedTerms[compute] missing %q; got %v", want, computeTerms)
		}
	}
}

// TestPriceLadder_ISP_Cheaper_Than_CSP verifies that for the same instance type
// and region, ISP gives a lower rate than CSP (expected from fixture and real data).
func TestPriceLadder_ISP_Cheaper_Than_CSP(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	payOpt := "No Upfront"
	years := 1

	specCSP := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermComputeSP,
		},
		ResourceType:    "m5.xlarge",
		OS:              "Linux",
		PaymentOption:   &payOpt,
		CommitmentYears: &years,
	}

	specISP := *specCSP
	specISP.Term = models.PricingTermEC2InstanceSP

	rCSP, err := p.GetSavingsPlanPrice(context.Background(), specCSP)
	if err != nil || len(rCSP.PublicPrices) == 0 {
		t.Fatalf("CSP price: err=%v", err)
	}
	rISP, err := p.GetSavingsPlanPrice(context.Background(), &specISP)
	if err != nil || len(rISP.PublicPrices) == 0 {
		t.Fatalf("ISP price: err=%v", err)
	}

	cspRate := rCSP.PublicPrices[0].PricePerUnit
	ispRate := rISP.PublicPrices[0].PricePerUnit
	if ispRate >= cspRate {
		t.Errorf("ISP rate (%v) should be lower than CSP rate (%v) for same instance/region", ispRate, cspRate)
	}
}

// TestCacheHit_AvoidSecondHTTPFetch verifies that a second call to getSPIndex
// for the same region hits the cache and does NOT make additional HTTP requests.
func TestCacheHit_AvoidSecondHTTPFetch(t *testing.T) {
	fetchCount := 0

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		switch {
		case strings.HasSuffix(r.URL.Path, "/region_index.json"):
			idx := map[string]any{
				"regions": []map[string]string{
					{
						"regionCode": "us-east-1",
						"versionUrl": srv.URL + "/test-version/us-east-1/index.json",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(idx)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(minimalSPFixtureJSON))
		}
	}))
	defer srv.Close()

	p := newTestProvider(t)
	overrideSPBaseURL(t, srv.URL)

	// First call — should fetch region index + per-region file.
	_, err := p.getSPIndex(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("first getSPIndex: %v", err)
	}
	firstCount := fetchCount

	// Second call — should hit cache, no HTTP.
	_, err = p.getSPIndex(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("second getSPIndex: %v", err)
	}
	if fetchCount != firstCount {
		t.Errorf("cache miss on second call: HTTP fetches went from %d to %d", firstCount, fetchCount)
	}
}

// TestWindowsOperationCode_Mapping verifies that Windows OS maps to the correct
// operation code in the SP rate lookup.
func TestWindowsOperationCode_Mapping(t *testing.T) {
	p, srv := setupSPTest(t, "us-east-1")
	defer srv.Close()

	p.bulkFallback = true

	payOpt := "No Upfront"
	years := 1
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermComputeSP,
		},
		ResourceType:    "m5.xlarge",
		OS:              "Windows",
		PaymentOption:   &payOpt,
		CommitmentYears: &years,
	}

	result, err := p.GetSavingsPlanPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetSavingsPlanPrice Windows: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatalf("expected Windows SP price, got empty (note=%q)", result.Note)
	}
	// The fixture has Windows rate at 0.2800.
	got := result.PublicPrices[0].PricePerUnit
	const wantWindows = 0.2800
	if got != wantWindows {
		t.Errorf("Windows CSP rate = %v, want %v", got, wantWindows)
	}
	// Verify the operation attribute is correct.
	if op, ok := result.PublicPrices[0].Attributes["operation"]; !ok || op != "RunInstances:0010" {
		t.Errorf("operation attribute = %q, want RunInstances:0010", op)
	}
}
