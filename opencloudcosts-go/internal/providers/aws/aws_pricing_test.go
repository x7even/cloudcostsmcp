package aws

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Test fixtures — ported from Python test_providers/test_aws.py
// --------------------------------------------------------------------------

// m5XLargeOnDemandJSON is a minimal AWS pricing API response for m5.xlarge.
const m5XLargeOnDemandJSON = `{
  "product": {
    "sku": "JRTCKXETXF8Z6NMQ",
    "productFamily": "Compute Instance",
    "attributes": {
      "instanceType": "m5.xlarge",
      "vcpu": "4",
      "memory": "16 GiB",
      "operatingSystem": "Linux",
      "tenancy": "Shared",
      "location": "US East (N. Virginia)",
      "preInstalledSw": "NA",
      "capacitystatus": "Used",
      "networkPerformance": "Up to 10 Gigabit",
      "storage": "EBS only"
    }
  },
  "terms": {
    "OnDemand": {
      "JRTCKXETXF8Z6NMQ.JRTCKXETXF": {
        "priceDimensions": {
          "JRTCKXETXF8Z6NMQ.JRTCKXETXF.6YS6EN2CT7": {
            "unit": "Hrs",
            "pricePerUnit": {"USD": "0.1920000000"},
            "description": "$0.192 per On Demand Linux m5.xlarge Instance Hour"
          }
        },
        "termAttributes": {}
      }
    }
  }
}`

// gp3StorageJSON is a minimal storage pricing response.
const gp3StorageJSON = `{
  "product": {
    "sku": "STORAGESKUID1",
    "productFamily": "Storage",
    "attributes": {
      "volumeType": "General Purpose",
      "volumeApiName": "gp3",
      "location": "US East (N. Virginia)",
      "storageMedia": "SSD-backed"
    }
  },
  "terms": {
    "OnDemand": {
      "STORAGESKUID1.JRTCKXETXF": {
        "priceDimensions": {
          "STORAGESKUID1.JRTCKXETXF.6YS6EN2CT7": {
            "unit": "GB-Mo",
            "pricePerUnit": {"USD": "0.0800000000"},
            "description": "$0.08 per GB-month of General Purpose (SSD) provisioned storage"
          }
        },
        "termAttributes": {}
      }
    }
  }
}`

// mysqlRDSJSON is a minimal RDS pricing response for MySQL.
const mysqlRDSJSON = `{
  "product": {
    "sku": "RDSSKU123",
    "productFamily": "Database Instance",
    "attributes": {
      "instanceType": "db.r5.large",
      "databaseEngine": "MySQL",
      "deploymentOption": "Single-AZ",
      "location": "US East (N. Virginia)"
    }
  },
  "terms": {
    "OnDemand": {
      "RDSSKU123.JRTCKXETXF": {
        "priceDimensions": {
          "RDSSKU123.JRTCKXETXF.6YS6EN2CT7": {
            "unit": "Hrs",
            "pricePerUnit": {"USD": "0.2400000000"},
            "description": "$0.24 per RDS db.r5.large MySQL instance hour"
          }
        },
        "termAttributes": {}
      }
    }
  }
}`

// reserved1yrJSON is a minimal reserved pricing response.
const reserved1yrJSON = `{
  "product": {
    "sku": "RESERVEDSKU1",
    "productFamily": "Compute Instance",
    "attributes": {
      "instanceType": "m5.xlarge",
      "operatingSystem": "Linux",
      "tenancy": "Shared",
      "location": "US East (N. Virginia)",
      "preInstalledSw": "NA",
      "capacitystatus": "Used"
    }
  },
  "terms": {
    "Reserved": {
      "RESERVEDSKU1.4NA7Y494T4": {
        "priceDimensions": {
          "RESERVEDSKU1.4NA7Y494T4.6YS6EN2CT7": {
            "unit": "Hrs",
            "pricePerUnit": {"USD": "0.1140000000"},
            "description": "$0.114 per Reserved Instance hour"
          }
        },
        "termAttributes": {
          "LeaseContractLength": "1yr",
          "PurchaseOption": "No Upfront"
        }
      }
    }
  }
}`

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

// newTestProvider creates a Provider with a temp cache directory for tests.
// The pricing/ec2 clients are nil — tests that exercise them must either
// mock GetProducts directly or provide a real client.
func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	cfg := &config.Config{
		CacheTTLHours:   24,
		MetadataTTLDays: 7,
		AWSRegion:       "us-east-1",
	}
	return &Provider{
		cfg:   cfg,
		cache: cm,
		// pricingClient and ec2Client left nil; tests mock at a higher level.
	}
}

// --------------------------------------------------------------------------
// Unit tests for SKU parsing helpers
// --------------------------------------------------------------------------

func TestExtractOnDemandPrice(t *testing.T) {
	var sku parsedSKU
	if err := json.Unmarshal([]byte(m5XLargeOnDemandJSON), &sku); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	price, unit := extractOnDemandPrice(sku)
	if price != 0.192 {
		t.Errorf("price = %v, want 0.192", price)
	}
	if unit != "Hrs" {
		t.Errorf("unit = %q, want Hrs", unit)
	}
}

func TestExtractOnDemandPrice_ZeroReturnsZero(t *testing.T) {
	raw := `{
      "product": {"sku": "X", "productFamily": "Compute Instance", "attributes": {}},
      "terms": {
        "OnDemand": {
          "X.TERM": {
            "priceDimensions": {
              "X.TERM.DIM": {"unit": "Hrs", "pricePerUnit": {"USD": "0.0000000000"}, "description": ""}
            },
            "termAttributes": {}
          }
        }
      }
    }`
	var sku parsedSKU
	if err := json.Unmarshal([]byte(raw), &sku); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	price, _ := extractOnDemandPrice(sku)
	if price != 0 {
		t.Errorf("expected 0 for zero-price SKU, got %v", price)
	}
}

func TestExtractReservedPrice_1yr(t *testing.T) {
	var sku parsedSKU
	if err := json.Unmarshal([]byte(reserved1yrJSON), &sku); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	price, unit := extractReservedPrice(sku, models.PricingTermReserved1Yr)
	if price != 0.114 {
		t.Errorf("price = %v, want 0.114", price)
	}
	if unit != "Hrs" {
		t.Errorf("unit = %q, want Hrs", unit)
	}
}

func TestExtractReservedPrice_WrongTerm(t *testing.T) {
	var sku parsedSKU
	if err := json.Unmarshal([]byte(reserved1yrJSON), &sku); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Reserved 3yr should not match a 1yr term in the JSON
	price, _ := extractReservedPrice(sku, models.PricingTermReserved3Yr)
	if price != 0 {
		t.Errorf("expected 0 for wrong term, got %v", price)
	}
}

func TestSKUToNormalizedPrice_OnDemand(t *testing.T) {
	np := skuToNormalizedPrice(m5XLargeOnDemandJSON, "us-east-1", models.PricingTermOnDemand, "compute")
	if np == nil {
		t.Fatal("expected non-nil NormalizedPrice")
	}
	if np.Provider != models.CloudProviderAWS {
		t.Errorf("provider = %v, want aws", np.Provider)
	}
	if np.PricingTerm != models.PricingTermOnDemand {
		t.Errorf("term = %v, want on_demand", np.PricingTerm)
	}
	if np.PricePerUnit != 0.192 {
		t.Errorf("price = %v, want 0.192", np.PricePerUnit)
	}
	if np.Unit != models.PriceUnitPerHour {
		t.Errorf("unit = %v, want per_hour", np.Unit)
	}
	if np.Region != "us-east-1" {
		t.Errorf("region = %v, want us-east-1", np.Region)
	}
	if np.SKUID != "JRTCKXETXF8Z6NMQ" {
		t.Errorf("sku_id = %v, want JRTCKXETXF8Z6NMQ", np.SKUID)
	}
	// Verify attributes are populated
	if np.Attributes["instanceType"] != "m5.xlarge" {
		t.Errorf("attributes[instanceType] = %q, want m5.xlarge", np.Attributes["instanceType"])
	}
	if np.Attributes["vcpu"] != "4" {
		t.Errorf("attributes[vcpu] = %q, want 4", np.Attributes["vcpu"])
	}
	// Noise keys should be stripped
	if _, ok := np.Attributes["location"]; ok {
		t.Error("expected location to be stripped from attributes")
	}
}

func TestSKUToNormalizedPrice_NoPrice(t *testing.T) {
	raw := `{"product": {"sku": "X", "productFamily": "Compute Instance", "attributes": {}}, "terms": {}}`
	np := skuToNormalizedPrice(raw, "us-east-1", models.PricingTermOnDemand, "compute")
	if np != nil {
		t.Errorf("expected nil for SKU with no price, got %+v", np)
	}
}

func TestParseUnit(t *testing.T) {
	tests := []struct {
		input string
		want  models.PriceUnit
	}{
		{"Hrs", models.PriceUnitPerHour},
		{"hrs", models.PriceUnitPerHour},
		{"GB-Mo", models.PriceUnitPerGBMonth},
		{"GB", models.PriceUnitPerGB},
		{"IOPS-Mo", models.PriceUnitPerIOPSMonth},
		{"Requests", models.PriceUnitPerRequest},
		{"GB-Second", models.PriceUnitPerGBSecond},
		{"something-else", models.PriceUnitPerUnit},
	}
	for _, tc := range tests {
		got := parseUnit(tc.input)
		if got != tc.want {
			t.Errorf("parseUnit(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// Table-driven tests for region mapping
// --------------------------------------------------------------------------

func TestRegionToLocation(t *testing.T) {
	tests := []struct {
		region  string
		wantLoc string
		wantErr bool
	}{
		{"us-east-1", "US East (N. Virginia)", false},
		{"eu-west-1", "Europe (Ireland)", false},
		{"ap-southeast-1", "Asia Pacific (Singapore)", false},
		{"invalid-region-99", "", true},
	}
	for _, tc := range tests {
		loc, err := regionToLocation(tc.region)
		if tc.wantErr {
			if err == nil {
				t.Errorf("regionToLocation(%q): expected error, got nil", tc.region)
			}
			continue
		}
		if err != nil {
			t.Errorf("regionToLocation(%q): unexpected error: %v", tc.region, err)
			continue
		}
		if loc != tc.wantLoc {
			t.Errorf("regionToLocation(%q) = %q, want %q", tc.region, loc, tc.wantLoc)
		}
	}
}

// --------------------------------------------------------------------------
// Table-driven tests for provider identity methods
// --------------------------------------------------------------------------

func TestProviderIdentity(t *testing.T) {
	p := newTestProvider(t)

	if got := p.Name(); got != models.CloudProviderAWS {
		t.Errorf("Name() = %v, want aws", got)
	}
	if got := p.DefaultRegion(); got != "us-east-1" {
		t.Errorf("DefaultRegion() = %v, want us-east-1", got)
	}

	major := p.MajorRegions()
	if len(major) != 12 {
		t.Errorf("MajorRegions(): got %d regions, want 12", len(major))
	}
	found := false
	for _, r := range major {
		if r == "us-east-1" {
			found = true
		}
	}
	if !found {
		t.Error("MajorRegions(): us-east-1 not present")
	}
}

func TestSupports(t *testing.T) {
	p := newTestProvider(t)
	tests := []struct {
		domain  models.PricingDomain
		service string
		want    bool
	}{
		{models.PricingDomainCompute, "ec2", true},
		{models.PricingDomainCompute, "fargate", true},
		{models.PricingDomainStorage, "ebs", true},
		{models.PricingDomainStorage, "s3", true},
		{models.PricingDomainDatabase, "rds", true},
		{models.PricingDomainNetwork, "lb", true},
		{models.PricingDomainNetwork, "egress", true},
		{models.PricingDomainCompute, "nonexistent-service", false},
	}
	for _, tc := range tests {
		got := p.Supports(tc.domain, tc.service)
		if got != tc.want {
			t.Errorf("Supports(%v, %q) = %v, want %v", tc.domain, tc.service, got, tc.want)
		}
	}
}

func TestSupportedTerms_Compute(t *testing.T) {
	p := newTestProvider(t)
	terms := p.SupportedTerms(models.PricingDomainCompute, "ec2")
	// Must include on_demand and reserved terms
	found := map[models.PricingTerm]bool{}
	for _, t := range terms {
		found[t] = true
	}
	for _, want := range []models.PricingTerm{
		models.PricingTermOnDemand,
		models.PricingTermReserved1Yr,
		models.PricingTermReserved3Yr,
	} {
		if !found[want] {
			t.Errorf("SupportedTerms(compute): missing %v", want)
		}
	}
}

func TestSupportedTerms_Storage(t *testing.T) {
	p := newTestProvider(t)
	terms := p.SupportedTerms(models.PricingDomainStorage, "ebs")
	if len(terms) != 1 || terms[0] != models.PricingTermOnDemand {
		t.Errorf("SupportedTerms(storage): got %v, want [on_demand]", terms)
	}
}

// --------------------------------------------------------------------------
// GetComputePrice with mock GetProducts
// --------------------------------------------------------------------------

// These tests use a pattern where we call the helper functions directly
// rather than going through the full method chain, to avoid needing real
// AWS clients.

func TestGetComputePrice_ParsesPrice(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Inject mock by directly pre-populating the cache with the expected result
	// We test GetComputePrice by first testing skuToNormalizedPrice (the core logic)
	// and separately testing the cache path.
	np := skuToNormalizedPrice(m5XLargeOnDemandJSON, "us-east-1", models.PricingTermOnDemand, "compute")
	if np == nil {
		t.Fatal("skuToNormalizedPrice returned nil")
	}

	// Pre-populate cache to test cache path
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	cacheKey := "aws:compute:us-east-1:m5.xlarge:Linux:on_demand"
	ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
	p.cache.Set(cacheKey, data, ttl)

	// Now call GetComputePrice — should hit cache
	result, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d prices, want 1", len(result))
	}
	if result[0].PricePerUnit != 0.192 {
		t.Errorf("price = %v, want 0.192", result[0].PricePerUnit)
	}
	if result[0].Provider != models.CloudProviderAWS {
		t.Errorf("provider = %v, want aws", result[0].Provider)
	}
}

func TestGetComputePrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	np := skuToNormalizedPrice(m5XLargeOnDemandJSON, "us-east-1", models.PricingTermOnDemand, "compute")
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	cacheKey := "aws:compute:us-east-1:m5.xlarge:Linux:on_demand"
	p.cache.Set(cacheKey, data, 24*time.Hour)

	// Second call — should use cached value without hitting the API
	r1, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	r2, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(r1) != len(r2) {
		t.Errorf("cache inconsistency: first=%d, second=%d", len(r1), len(r2))
	}
}

func TestGetComputePrice_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetComputePrice(context.Background(), "m5.xlarge", "invalid-region-99", "Linux", models.PricingTermOnDemand)
	if err == nil {
		t.Fatal("expected error for invalid region, got nil")
	}
}

// --------------------------------------------------------------------------
// GetStoragePrice
// --------------------------------------------------------------------------

func TestGetStoragePrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	np := skuToNormalizedPrice(gp3StorageJSON, "us-east-1", models.PricingTermOnDemand, "storage")
	if np == nil {
		t.Fatal("skuToNormalizedPrice returned nil for gp3 storage")
	}
	np.Unit = models.PriceUnitPerGBMonth // simulate unit fixup

	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	cacheKey := "aws:storage:us-east-1:gp3"
	p.cache.Set(cacheKey, data, 24*time.Hour)

	result, err := p.GetStoragePrice(ctx, "gp3", "us-east-1", 100)
	if err != nil {
		t.Fatalf("GetStoragePrice: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d prices, want 1", len(result))
	}
	if result[0].Unit != models.PriceUnitPerGBMonth {
		t.Errorf("unit = %v, want per_gb_month", result[0].Unit)
	}
}

func TestGetStoragePrice_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetStoragePrice(context.Background(), "gp3", "bad-region-x", 0)
	if err == nil {
		t.Fatal("expected error for invalid region")
	}
}

// --------------------------------------------------------------------------
// GetDatabasePrice
// --------------------------------------------------------------------------

func TestGetDatabasePrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	np := skuToNormalizedPrice(mysqlRDSJSON, "us-east-1", models.PricingTermOnDemand, "database")
	if np == nil {
		t.Fatal("skuToNormalizedPrice returned nil for RDS")
	}
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	cacheKey := "aws:database:us-east-1:mysql:db.r5.large:on_demand"
	p.cache.Set(cacheKey, data, 24*time.Hour)

	result, err := p.GetDatabasePrice(ctx, "mysql", "db.r5.large", "us-east-1", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetDatabasePrice: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d prices, want 1", len(result))
	}
	if result[0].PricePerUnit != 0.24 {
		t.Errorf("price = %v, want 0.24", result[0].PricePerUnit)
	}
}

func TestGetDatabasePrice_EngineNormalization(t *testing.T) {
	// Test that engine aliases resolve correctly
	tests := []struct {
		input string
		want  string
	}{
		{"mysql", "MySQL"},
		{"postgres", "PostgreSQL"},
		{"postgresql", "PostgreSQL"},
		{"mariadb", "MariaDB"},
		{"aurora-mysql", "Aurora MySQL"},
		{"aurora-postgresql", "Aurora PostgreSQL"},
		{"aurora-postgres", "Aurora PostgreSQL"},
	}
	for _, tc := range tests {
		got := normalizeRDSEngine(tc.input)
		if got != tc.want {
			t.Errorf("normalizeRDSEngine(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// CheckAvailability
// --------------------------------------------------------------------------

func TestCheckAvailability_CacheHit_Available(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Populate cache with a price so CheckAvailability returns true
	np := skuToNormalizedPrice(m5XLargeOnDemandJSON, "us-east-1", models.PricingTermOnDemand, "compute")
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	cacheKey := "aws:compute:us-east-1:m5.xlarge:Linux:on_demand"
	p.cache.Set(cacheKey, data, 24*time.Hour)

	avail, alts, err := p.CheckAvailability(ctx, "compute", "m5.xlarge", "us-east-1")
	if err != nil {
		t.Fatalf("CheckAvailability: %v", err)
	}
	if !avail {
		t.Error("expected available=true")
	}
	if alts != nil {
		t.Errorf("expected nil alternates, got %v", alts)
	}
}

func TestCheckAvailability_NotInCache(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Cache is empty; pricingClient is nil so GetProducts will fail.
	// We expect GetComputePrice to error or return empty (unavailable).
	// Since pricingClient is nil, GetProducts will panic or error —
	// let's check the region error path first.
	avail, _, err := p.CheckAvailability(ctx, "compute", "m99.fake", "invalid-region-zz")
	if err == nil && avail {
		t.Error("expected either error or unavailable for bad region")
	}
}

// --------------------------------------------------------------------------
// ListRegions fallback (no real client)
// --------------------------------------------------------------------------

func TestListRegions_FallbackToStaticMap(t *testing.T) {
	p := newTestProvider(t)
	// ec2Client is nil, so DescribeRegions will fail.
	// ListRegions should fall back to static map.
	ctx := context.Background()
	regions, err := p.ListRegions(ctx, "compute")
	if err != nil {
		t.Fatalf("ListRegions: %v", err)
	}
	if len(regions) == 0 {
		t.Error("ListRegions returned empty slice")
	}
	found := false
	for _, r := range regions {
		if r == "us-east-1" {
			found = true
		}
	}
	if !found {
		t.Error("ListRegions: us-east-1 not in result")
	}
}

// --------------------------------------------------------------------------
// GetPrice dispatcher
// --------------------------------------------------------------------------

func TestGetPrice_Storage_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	np := skuToNormalizedPrice(gp3StorageJSON, "us-east-1", models.PricingTermOnDemand, "storage")
	np.Unit = models.PriceUnitPerGBMonth
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:storage:us-east-1:gp3", data, 24*time.Hour)

	sizeGB := 100.0
	spec := &models.StoragePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainStorage,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		StorageType: "gp3",
		SizeGB:      &sizeGB,
	}

	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if result == nil {
		t.Fatal("GetPrice returned nil result")
	}
	if len(result.PublicPrices) == 0 {
		t.Error("expected at least one public price")
	}
}

func TestGetPrice_UnsupportedDomain(t *testing.T) {
	p := newTestProvider(t)
	spec := &models.AiPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainAI,
			Region:   "us-east-1",
		},
	}
	_, err := p.GetPrice(context.Background(), spec)
	if err == nil {
		t.Error("expected error for unsupported domain in Part 1")
	}
}

// --------------------------------------------------------------------------
// FinOps stubs
// --------------------------------------------------------------------------

func TestGetEffectivePrice_ReturnsErrNotSupported(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetEffectivePrice(context.Background(), nil)
	if err == nil {
		t.Error("expected ErrNotSupported")
	}
}

func TestGetDiscountSummary_ReturnsErrNotSupported(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetDiscountSummary(context.Background())
	if err == nil {
		t.Error("expected ErrNotSupported")
	}
}

func TestGetSpotHistory_NonComputeSpec(t *testing.T) {
	// GetSpotHistory (implemented in aws_finops.go) returns ErrNotSupported
	// when the spec is not a *ComputePricingSpec with resource_type set.
	p := newTestProvider(t)
	_, err := p.GetSpotHistory(context.Background(), nil, 24, "")
	if err == nil {
		t.Error("GetSpotHistory with nil spec: expected error, got nil")
	}
}

func TestDescribeCatalog_ReturnsAWSProvider(t *testing.T) {
	// DescribeCatalog is implemented in aws_finops.go and returns a full catalog.
	p := newTestProvider(t)
	cat, err := p.DescribeCatalog(context.Background())
	if err != nil {
		t.Fatalf("DescribeCatalog: %v", err)
	}
	if cat.Provider != models.CloudProviderAWS {
		t.Errorf("catalog provider = %v, want aws", cat.Provider)
	}
}

// --------------------------------------------------------------------------
// EBS type mapping
// --------------------------------------------------------------------------

func TestMapEBSType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gp2", "General Purpose"},
		{"gp3", "General Purpose"},
		{"io1", "Provisioned IOPS"},
		{"io2", "Provisioned IOPS"},
		{"st1", "Throughput Optimized HDD"},
		{"sc1", "Cold HDD"},
		{"standard", "Magnetic"},
		{"GP3", "General Purpose"}, // case-insensitive
	}
	for _, tc := range tests {
		got := mapEBSType(tc.input)
		if got != tc.want {
			t.Errorf("mapEBSType(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// --------------------------------------------------------------------------
// Network/Egress tests (aws_network.go)
// --------------------------------------------------------------------------

func TestRegionContinent(t *testing.T) {
	tests := []struct {
		region string
		want   string
	}{
		{"us-east-1", "us"},
		{"us-west-2", "us"},
		{"ca-central-1", "us"},
		{"eu-west-1", "eu"},
		{"eu-central-1", "eu"},
		{"ap-southeast-1", "ap"},
		{"ap-northeast-1", "ap"},
		{"sa-east-1", "sa"},
		{"me-south-1", "me"},
		{"af-south-1", "af"},
		{"unknown-region", "us"}, // default
	}
	for _, tc := range tests {
		got := regionContinent(tc.region)
		if got != tc.want {
			t.Errorf("regionContinent(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

func TestInterRegionRate(t *testing.T) {
	tests := []struct {
		src  string
		dst  string
		want float64
	}{
		{"us-east-1", "us-west-2", 0.02},      // us→us intra
		{"us-east-1", "eu-west-1", 0.02},      // us→eu
		{"us-east-1", "ap-southeast-1", 0.09}, // us→ap
		{"us-east-1", "sa-east-1", 0.16},      // us→sa
		{"eu-west-1", "ap-southeast-1", 0.09}, // eu→ap
		{"ap-southeast-1", "ap-northeast-1", 0.09}, // ap→ap
	}
	for _, tc := range tests {
		got := interRegionRate(tc.src, tc.dst)
		if got != tc.want {
			t.Errorf("interRegionRate(%q, %q) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}

func TestEgressStaticFallback_WithDest(t *testing.T) {
	now := time.Now()
	prices := egressStaticFallback("us-east-1", "eu-west-1", now)
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}
	p := prices[0]
	if p.Provider != models.CloudProviderAWS {
		t.Errorf("provider = %v, want aws", p.Provider)
	}
	if p.PricePerUnit != 0.02 {
		t.Errorf("price = %v, want 0.02 (us→eu)", p.PricePerUnit)
	}
	if p.Unit != models.PriceUnitPerGB {
		t.Errorf("unit = %v, want per_gb", p.Unit)
	}
	if p.Attributes["fallback"] != "true" {
		t.Errorf("expected fallback=true in attributes")
	}
}

func TestEgressStaticFallback_NoDest(t *testing.T) {
	now := time.Now()
	// No dest — should return multiple entries (one per destination continent from us-east-1).
	prices := egressStaticFallback("us-east-1", "", now)
	if len(prices) == 0 {
		t.Fatal("expected >0 prices for no-dest fallback")
	}
	// All should have fromRegionCode = us-east-1.
	for _, p := range prices {
		if p.Attributes["fromRegionCode"] != "us-east-1" {
			t.Errorf("fromRegionCode = %q, want us-east-1", p.Attributes["fromRegionCode"])
		}
		if p.Unit != models.PriceUnitPerGB {
			t.Errorf("unit = %v, want per_gb", p.Unit)
		}
	}
}

func TestGetNetworkPrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Pre-populate cache with static prices.
	now := time.Now()
	prices := egressStaticFallback("us-east-1", "eu-west-1", now)
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:network_egress:us-east-1:eu-west-1", data, 24*time.Hour)

	spec := &models.EgressPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainInterRegionEgress,
			Region:   "us-east-1",
		},
		SourceRegion: "us-east-1",
		DestRegion:   "eu-west-1",
	}
	result, err := p.GetNetworkPrice(ctx, spec, "us-east-1")
	if err != nil {
		t.Fatalf("GetNetworkPrice: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected at least one price")
	}
	if result[0].PricePerUnit != 0.02 {
		t.Errorf("price = %v, want 0.02", result[0].PricePerUnit)
	}
}

func TestGetNetworkPrice_FallsBackToStatic(t *testing.T) {
	// With no cache and pricingClient nil, GetProducts will fail.
	// GetNetworkPrice should silently fall back to static rates.
	p := newTestProvider(t)
	ctx := context.Background()

	spec := &models.EgressPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainInterRegionEgress,
			Region:   "us-east-1",
		},
		SourceRegion: "us-east-1",
		DestRegion:   "ap-southeast-1",
	}
	result, err := p.GetNetworkPrice(ctx, spec, "us-east-1")
	if err != nil {
		t.Fatalf("GetNetworkPrice: unexpected error: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected static fallback prices, got none")
	}
	// us→ap rate should be 0.09
	if result[0].PricePerUnit != 0.09 {
		t.Errorf("price = %v, want 0.09 (us→ap)", result[0].PricePerUnit)
	}
}

// --------------------------------------------------------------------------
// Lambda/Serverless tests (aws_network.go)
// --------------------------------------------------------------------------

func TestGetLambdaPrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	now := time.Now()
	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderAWS,
			Service:       "serverless",
			SKUID:         "aws:lambda:us-east-1:requests",
			ProductFamily: "AWS Lambda",
			Description:   "Lambda requests — $0.20 per 1M requests",
			Region:        "us-east-1",
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.0000002,
			Unit:          models.PriceUnitPerRequest,
			Currency:      "USD",
			FetchedAt:     &now,
		},
		{
			Provider:      models.CloudProviderAWS,
			Service:       "serverless",
			SKUID:         "aws:lambda:us-east-1:duration",
			ProductFamily: "AWS Lambda",
			Description:   "Lambda duration (per GB-second)",
			Region:        "us-east-1",
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.0000166667,
			Unit:          models.PriceUnitPerGBSecond,
			Currency:      "USD",
			FetchedAt:     &now,
		},
	}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:lambda:us-east-1", data, 24*time.Hour)

	result, err := p.GetLambdaPrice(ctx, "us-east-1")
	if err != nil {
		t.Fatalf("GetLambdaPrice: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 prices (requests + duration), got %d", len(result))
	}

	// Check request price
	req := result[0]
	if req.Unit != models.PriceUnitPerRequest {
		t.Errorf("requests price unit = %v, want per_request", req.Unit)
	}
	if req.PricePerUnit != 0.0000002 {
		t.Errorf("requests price = %v, want 0.0000002", req.PricePerUnit)
	}

	// Check duration price
	dur := result[1]
	if dur.Unit != models.PriceUnitPerGBSecond {
		t.Errorf("duration price unit = %v, want per_gb_second", dur.Unit)
	}
}

func TestGetLambdaPrice_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetLambdaPrice(context.Background(), "bad-region-x")
	if err == nil {
		t.Fatal("expected error for invalid region")
	}
}

// --------------------------------------------------------------------------
// ElastiCache tests (aws_network.go)
// --------------------------------------------------------------------------

// elasticacheSampleJSON is a minimal AmazonElastiCache pricing response.
const elasticacheSampleJSON = `{
  "product": {
    "sku": "ECSKUID1",
    "productFamily": "Cache Instance",
    "attributes": {
      "instanceType": "cache.r6g.large",
      "cacheEngine": "Redis",
      "location": "US East (N. Virginia)"
    }
  },
  "terms": {
    "OnDemand": {
      "ECSKUID1.JRTCKXETXF": {
        "priceDimensions": {
          "ECSKUID1.JRTCKXETXF.6YS6EN2CT7": {
            "unit": "Hrs",
            "pricePerUnit": {"USD": "0.1560000000"},
            "description": "$0.156 per ElastiCache cache.r6g.large instance hour"
          }
        },
        "termAttributes": {}
      }
    }
  }
}`

func TestGetElastiCachePrice_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	np := skuToNormalizedPrice(elasticacheSampleJSON, "us-east-1", models.PricingTermOnDemand, "database")
	if np == nil {
		t.Fatal("skuToNormalizedPrice returned nil for ElastiCache")
	}
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:elasticache:us-east-1:cache.r6g.large", data, 24*time.Hour)

	result, err := p.GetElastiCachePrice(ctx, "cache.r6g.large", "us-east-1")
	if err != nil {
		t.Fatalf("GetElastiCachePrice: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d prices, want 1", len(result))
	}
	if result[0].PricePerUnit != 0.156 {
		t.Errorf("price = %v, want 0.156", result[0].PricePerUnit)
	}
	if result[0].Unit != models.PriceUnitPerHour {
		t.Errorf("unit = %v, want per_hour", result[0].Unit)
	}
}

func TestGetElastiCachePrice_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetElastiCachePrice(context.Background(), "cache.r6g.large", "bad-region-zz")
	if err == nil {
		t.Fatal("expected error for invalid region")
	}
}

// TestGetDatabasePrice_ElastiCacheRouting verifies that GetDatabasePrice routes
// elasticache/redis/memcached engine aliases to GetElastiCachePrice.
func TestGetDatabasePrice_ElastiCacheRouting(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Pre-populate the ElastiCache cache key.
	np := skuToNormalizedPrice(elasticacheSampleJSON, "us-east-1", models.PricingTermOnDemand, "database")
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:elasticache:us-east-1:cache.r6g.large", data, 24*time.Hour)

	engineAliases := []string{
		"redis", "memcached", "elasticache", "elasticache-redis", "elasticache-memcached",
	}
	for _, engine := range engineAliases {
		result, err := p.GetDatabasePrice(ctx, engine, "cache.r6g.large", "us-east-1", models.PricingTermOnDemand)
		if err != nil {
			t.Errorf("GetDatabasePrice(engine=%q): unexpected error: %v", engine, err)
			continue
		}
		if len(result) == 0 {
			t.Errorf("GetDatabasePrice(engine=%q): expected prices, got none", engine)
		}
	}
}

// --------------------------------------------------------------------------
// GetPrice dispatcher tests for new domains
// --------------------------------------------------------------------------

func TestGetPrice_InterRegionEgress_FallsBackToStatic(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	spec := &models.EgressPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainInterRegionEgress,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		SourceRegion: "us-east-1",
		DestRegion:   "eu-west-1",
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice(inter_region_egress): %v", err)
	}
	if result == nil || len(result.PublicPrices) == 0 {
		t.Fatal("expected public prices for inter_region_egress")
	}
}

func TestGetPrice_Serverless_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	spec := &models.ServerlessPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainServerless,
			Region:   "bad-region-zz",
			Term:     models.PricingTermOnDemand,
		},
	}
	_, err := p.GetPrice(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for serverless with invalid region")
	}
}

func TestGetPrice_Serverless_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	now := time.Now()
	prices := []models.NormalizedPrice{{
		Provider:     models.CloudProviderAWS,
		Service:      "serverless",
		SKUID:        "aws:lambda:us-east-1:requests",
		Region:       "us-east-1",
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: 0.0000002,
		Unit:         models.PriceUnitPerRequest,
		Currency:     "USD",
		FetchedAt:    &now,
	}}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:lambda:us-east-1", data, 24*time.Hour)

	spec := &models.ServerlessPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainServerless,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice(serverless): %v", err)
	}
	if result == nil || len(result.PublicPrices) == 0 {
		t.Fatal("expected public prices for serverless")
	}
	if result.PublicPrices[0].Unit != models.PriceUnitPerRequest {
		t.Errorf("unit = %v, want per_request", result.PublicPrices[0].Unit)
	}
}

// --------------------------------------------------------------------------
// Ensure unused import is satisfied in tests
// --------------------------------------------------------------------------

var _ = os.DevNull // ensure os import is not elided
