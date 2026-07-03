package aws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	azureprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/azure"
	gcpprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/gcp"
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

// allUpfrontJSON is a minimal All-Upfront reserved pricing response where the
// hourly dimension is $0 and the full cost is the Quantity/upfront dimension.
// This mirrors _ALL_UPFRONT_ITEM from the Python tests.
const allUpfrontJSON = `{
  "product": {
    "sku": "ALLUPSFRONT1",
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
      "ALLUPSFRONT1.KEY.ALL": {
        "priceDimensions": {
          "dim1": {
            "unit": "Hrs",
            "pricePerUnit": {"USD": "0.0000000000"},
            "description": "$0.00 per Reserved Linux m5.xlarge Instance Hour"
          },
          "dim2": {
            "unit": "Quantity",
            "pricePerUnit": {"USD": "560.0000000000"},
            "description": "Upfront Fee"
          }
        },
        "termAttributes": {
          "LeaseContractLength": "1yr",
          "PurchaseOption": "All Upfront",
          "OfferingClass": "standard"
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

// TestExtractReservedPrice_AllUpfront_Normalised verifies that when an All-Upfront
// reserved instance has a $0/hr dimension and a $560 upfront Quantity dimension,
// extractReservedPrice returns $560/8760 ≈ $0.0639/hr — not $0/hr.
// This is the Go equivalent of test_reserved_1yr_all_upfront_normalised in Python.
func TestExtractReservedPrice_AllUpfront_Normalised(t *testing.T) {
	var sku parsedSKU
	if err := json.Unmarshal([]byte(allUpfrontJSON), &sku); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	price, unit := extractReservedPrice(sku, models.PricingTermReserved1YrAll)
	if unit != "Hrs" {
		t.Errorf("unit = %q, want Hrs", unit)
	}
	// Expected: $560 / 8760 ≈ $0.063927...
	expected := 560.0 / 8760.0
	if price == 0 {
		t.Fatal("All-Upfront reserved price must not be $0/hr — upfront cost must be divided by 8760")
	}
	const tolerance = 1e-6
	diff := price - expected
	if diff < -tolerance || diff > tolerance {
		t.Errorf("All-Upfront effective hourly = %v, want %v (560/8760)", price, expected)
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
		{"us-east-1", "us-west-2", 0.02},           // us→us intra
		{"us-east-1", "eu-west-1", 0.02},           // us→eu
		{"us-east-1", "ap-southeast-1", 0.09},      // us→ap
		{"us-east-1", "sa-east-1", 0.16},           // us→sa
		{"eu-west-1", "ap-southeast-1", 0.09},      // eu→ap
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

// TestGetPrice_ElastiCache_ServiceRouting verifies that GetPrice correctly routes
// database requests to ElastiCache when service="elasticache" is set but engine
// is omitted (defaults to "MySQL" from DatabasePricingSpec). This is the exact
// pattern the model uses: service="elasticache", no explicit engine field.
func TestGetPrice_ElastiCache_ServiceRouting(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Pre-populate the ElastiCache cache key with a valid price.
	np := skuToNormalizedPrice(elasticacheSampleJSON, "us-east-1", models.PricingTermOnDemand, "database")
	prices := []models.NormalizedPrice{*np}
	data, _ := json.Marshal(prices)
	p.cache.Set("aws:elasticache:us-east-1:cache.r6g.large", data, 24*time.Hour)

	// Simulate exactly what the model sends: service="elasticache", no engine.
	spec := &models.DatabasePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainDatabase,
			Region:   "us-east-1",
			Service:  "elasticache",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "cache.r6g.large",
		Engine:       "MySQL", // default — intentionally wrong, service should override
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice(service=elasticache, engine=MySQL): unexpected error: %v", err)
	}
	if result == nil || len(result.PublicPrices) == 0 {
		t.Fatal("GetPrice(service=elasticache, engine=MySQL): expected ElastiCache price, got empty — routing bug: service field not overriding default engine")
	}
	if result.PublicPrices[0].PricePerUnit != 0.156 {
		t.Errorf("price = %v, want 0.156", result.PublicPrices[0].PricePerUnit)
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
// TestNetworkEgress_Internet_TieredBreakdown — filter string regression
// --------------------------------------------------------------------------

// TestNetworkEgress_Internet_FilterValue verifies that GetNetworkPrice uses
// exactly "InterRegion Outbound" as the transferType filter value (not
// "AWS Inter-Region Outbound" or any other variant). The wrong string silently
// returns no API results and falls back to static rates.
//
// This is the Go equivalent of test_price_egress_uses_correct_filter in Python.
//
// The test serves a bulk pricing JSON containing a single AWSDataTransfer product
// whose transferType attribute is "InterRegion Outbound". If the filter in Go uses
// the correct string, getProductsBulk will match the product and GetNetworkPrice
// will return an API-sourced price (no fallback attribute). If the string is
// wrong, no product matches and the result falls back to static rates (with
// fallback="true" attribute).
func TestNetworkEgress_Internet_FilterValue(t *testing.T) {
	// Minimal AWSDataTransfer bulk JSON with a single egress product.
	// The transferType attribute must be "InterRegion Outbound" — the exact string
	// the AWS Pricing API uses. Any other string would not be returned by the API.
	const dataTransferBulkJSON = `{
"formatVersion": "aws_v1",
"products": {
  "EGRESS_SKU1": {
    "sku": "EGRESS_SKU1",
    "productFamily": "Data Transfer",
    "attributes": {
      "transferType": "InterRegion Outbound",
      "fromRegionCode": "us-east-1",
      "toRegionCode": "eu-west-1",
      "usagetype": "USE1-EU-AWS-Out-Bytes"
    }
  }
},
"terms": {
  "OnDemand": {
    "EGRESS_SKU1": {
      "EGRESS_SKU1.JRTCKXETXF": {
        "priceDimensions": {
          "EGRESS_SKU1.JRTCKXETXF.DIM": {
            "unit": "GB",
            "pricePerUnit": {"USD": "0.0200000000"},
            "description": "$0.020 per GB data transferred out"
          }
        },
        "termAttributes": {}
      }
    }
  },
  "Reserved": {}
}
}`

	// Start httptest server that serves the bulk JSON for any URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(dataTransferBulkJSON))
	}))
	defer srv.Close()

	// Override the bulk base URL so GetProducts uses our test server.
	origURL := bulkPricingBaseURL
	bulkPricingBaseURL = srv.URL
	defer func() { bulkPricingBaseURL = origURL }()

	// Create a provider in bulk-fallback mode (no pricing client).
	p := newTestProvider(t)
	p.bulkFallback = true

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
	prices, err := p.GetNetworkPrice(context.Background(), spec, "us-east-1")
	if err != nil {
		t.Fatalf("GetNetworkPrice: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price result")
	}

	// If the filter string is wrong, the bulk endpoint returns no matches and
	// GetNetworkPrice falls back to static rates (fallback="true").
	// A correct filter string matches our fixture and returns a live API price.
	for _, p := range prices {
		if p.Attributes["fallback"] == "true" {
			t.Error(`GetNetworkPrice fell back to static rates — the transferType filter ` +
				`value is wrong. Expected "InterRegion Outbound" but the filter ` +
				`did not match the bulk pricing product.`)
		}
	}

	// Also verify the price value matches our fixture ($0.02/GB).
	if prices[0].PricePerUnit != 0.02 {
		t.Errorf("price = %v, want 0.02 (from bulk fixture)", prices[0].PricePerUnit)
	}
}

// --------------------------------------------------------------------------
// Ensure unused import is satisfied in tests
// --------------------------------------------------------------------------

var _ = os.DevNull // ensure os import is not elided

// --------------------------------------------------------------------------
// Cross-term price invariant tests
//
// These tests use the bulk-fallback httptest pattern (p.bulkFallback=true) so
// that extractOnDemandPrice and extractReservedPrice are both exercised from a
// single shared SKU fixture — the cache pre-population approach would just
// compare two constants we typed rather than testing the parsing invariant.
// --------------------------------------------------------------------------

// newBulkProvider creates a Provider with bulkFallback=true.
// The caller must also call newBulkServer to override bulkPricingBaseURL.
func newBulkProvider(t *testing.T) *Provider {
	t.Helper()
	p := newTestProvider(t)
	p.bulkFallback = true
	return p
}

// newBulkServer starts an httptest.Server that serves body for any request,
// and overrides the package-level bulkPricingBaseURL to the server's URL.
// It registers a cleanup that closes the server and restores the original URL.
func newBulkServer(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	orig := bulkPricingBaseURL
	bulkPricingBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		bulkPricingBaseURL = orig
	})
}

// TestGetComputePrice_ReservedCheaperThanOnDemand verifies the core pricing
// invariant: for the same instance type, reserved_1yr must be cheaper than
// on_demand. A single bulk SKU carrying both OnDemand ($0.192/hr) and
// Reserved 1yr No-Upfront ($0.114/hr) terms is served via httptest so that
// extractOnDemandPrice and extractReservedPrice both run against real JSON.
func TestGetComputePrice_ReservedCheaperThanOnDemand(t *testing.T) {
	const bulkFixture = `{
  "formatVersion": "aws_v1",
  "products": {
    "INVSKU1": {
      "sku": "INVSKU1",
      "productFamily": "Compute Instance",
      "attributes": {
        "instanceType":    "m5.xlarge",
        "operatingSystem": "Linux",
        "tenancy":         "Shared",
        "preInstalledSw":  "NA",
        "capacitystatus":  "Used",
        "location":        "US East (N. Virginia)"
      }
    }
  },
  "terms": {
    "OnDemand": {
      "INVSKU1": {
        "INVSKU1.ODTERM": {
          "priceDimensions": {
            "INVSKU1.ODTERM.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.1920000000"},
              "description": "$0.192 per On Demand Linux m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {}
        }
      }
    },
    "Reserved": {
      "INVSKU1": {
        "INVSKU1.4NA7Y5ZX": {
          "priceDimensions": {
            "INVSKU1.4NA7Y5ZX.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.1140000000"},
              "description": "$0.114 per Reserved Linux m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "1yr",
            "PurchaseOption":      "No Upfront"
          }
        }
      }
    }
  }
}`

	newBulkServer(t, bulkFixture)
	p := newBulkProvider(t)
	ctx := context.Background()

	odPrices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice(on_demand): %v", err)
	}
	if len(odPrices) == 0 {
		t.Fatal("GetComputePrice(on_demand): expected at least one price, got none")
	}

	// Clear cache so the second call re-fetches (different cache key, so no issue).
	r1Prices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_1yr): %v", err)
	}
	if len(r1Prices) == 0 {
		t.Fatal("GetComputePrice(reserved_1yr): expected at least one price, got none")
	}

	odPrice := odPrices[0].PricePerUnit
	r1Price := r1Prices[0].PricePerUnit

	const tolerance = 1e-6
	if odPrice < 0.192-tolerance || odPrice > 0.192+tolerance {
		t.Errorf("on_demand price = %v, want ~0.192", odPrice)
	}
	if r1Price < 0.114-tolerance || r1Price > 0.114+tolerance {
		t.Errorf("reserved_1yr price = %v, want ~0.114", r1Price)
	}
	if r1Price >= odPrice {
		t.Errorf("invariant violation: reserved_1yr (%v) must be cheaper than on_demand (%v)", r1Price, odPrice)
	}
}

// TestGetComputePrice_Reserved3YrCheaperThan1Yr verifies the price ordering
// invariant: reserved_3yr must be cheaper than reserved_1yr for the same
// instance type. Both terms are served from a single bulk SKU fixture.
func TestGetComputePrice_Reserved3YrCheaperThan1Yr(t *testing.T) {
	const bulkFixture = `{
  "formatVersion": "aws_v1",
  "products": {
    "INVSKU2": {
      "sku": "INVSKU2",
      "productFamily": "Compute Instance",
      "attributes": {
        "instanceType":    "m5.xlarge",
        "operatingSystem": "Linux",
        "tenancy":         "Shared",
        "preInstalledSw":  "NA",
        "capacitystatus":  "Used",
        "location":        "US East (N. Virginia)"
      }
    }
  },
  "terms": {
    "OnDemand": {},
    "Reserved": {
      "INVSKU2": {
        "INVSKU2.R1NOTERM": {
          "priceDimensions": {
            "INVSKU2.R1NOTERM.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.1140000000"},
              "description": "$0.114 per Reserved 1yr No-Upfront m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "1yr",
            "PurchaseOption":      "No Upfront"
          }
        },
        "INVSKU2.R3NOTERM": {
          "priceDimensions": {
            "INVSKU2.R3NOTERM.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.0750000000"},
              "description": "$0.075 per Reserved 3yr No-Upfront m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "3yr",
            "PurchaseOption":      "No Upfront"
          }
        }
      }
    }
  }
}`

	newBulkServer(t, bulkFixture)
	p := newBulkProvider(t)
	ctx := context.Background()

	r1Prices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_1yr): %v", err)
	}
	if len(r1Prices) == 0 {
		t.Fatal("GetComputePrice(reserved_1yr): expected at least one price, got none")
	}

	r3Prices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved3Yr)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_3yr): %v", err)
	}
	if len(r3Prices) == 0 {
		t.Fatal("GetComputePrice(reserved_3yr): expected at least one price, got none")
	}

	r1Price := r1Prices[0].PricePerUnit
	r3Price := r3Prices[0].PricePerUnit

	const tolerance = 1e-6
	if r1Price < 0.114-tolerance || r1Price > 0.114+tolerance {
		t.Errorf("reserved_1yr price = %v, want ~0.114", r1Price)
	}
	if r3Price < 0.075-tolerance || r3Price > 0.075+tolerance {
		t.Errorf("reserved_3yr price = %v, want ~0.075", r3Price)
	}
	if r3Price >= r1Price {
		t.Errorf("invariant violation: reserved_3yr (%v) must be cheaper than reserved_1yr (%v)", r3Price, r1Price)
	}
}

// TestGetComputePrice_ReservedUpfrontOptions verifies the upfront payment
// ordering invariant: for the same lease length, more upfront commitment
// results in a cheaper effective hourly rate.
//
// For 1yr reserved instances: all-upfront < partial-upfront < no-upfront.
//
// The partial-upfront and all-upfront prices are normalised from a Quantity
// (upfront) dimension divided by (8760 * years). The fixture values are:
//   - no-upfront:      $0.114/hr   (hourly only)
//   - partial-upfront: $0.100/hr   ($0.050/hr + $438 upfront / 8760 = $0.050)
//   - all-upfront:     $0.089/hr   ($0.000/hr + $779.64 upfront / 8760 ≈ $0.089)
func TestGetComputePrice_ReservedUpfrontOptions(t *testing.T) {
	const bulkFixture = `{
  "formatVersion": "aws_v1",
  "products": {
    "INVSKU3": {
      "sku": "INVSKU3",
      "productFamily": "Compute Instance",
      "attributes": {
        "instanceType":    "m5.xlarge",
        "operatingSystem": "Linux",
        "tenancy":         "Shared",
        "preInstalledSw":  "NA",
        "capacitystatus":  "Used",
        "location":        "US East (N. Virginia)"
      }
    }
  },
  "terms": {
    "OnDemand": {},
    "Reserved": {
      "INVSKU3": {
        "INVSKU3.NOUPFRONT": {
          "priceDimensions": {
            "INVSKU3.NOUPFRONT.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.1140000000"},
              "description": "$0.114 per Reserved 1yr No-Upfront m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "1yr",
            "PurchaseOption":      "No Upfront"
          }
        },
        "INVSKU3.PARTIAL": {
          "priceDimensions": {
            "INVSKU3.PARTIAL.HRS": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.0500000000"},
              "description": "$0.050 per Reserved 1yr Partial-Upfront m5.xlarge Instance Hour"
            },
            "INVSKU3.PARTIAL.QTY": {
              "unit": "Quantity",
              "pricePerUnit": {"USD": "438.0000000000"},
              "description": "Upfront Fee"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "1yr",
            "PurchaseOption":      "Partial Upfront"
          }
        },
        "INVSKU3.ALL": {
          "priceDimensions": {
            "INVSKU3.ALL.HRS": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.0000000000"},
              "description": "$0.000 per Reserved 1yr All-Upfront m5.xlarge Instance Hour"
            },
            "INVSKU3.ALL.QTY": {
              "unit": "Quantity",
              "pricePerUnit": {"USD": "779.6400000000"},
              "description": "Upfront Fee"
            }
          },
          "termAttributes": {
            "LeaseContractLength": "1yr",
            "PurchaseOption":      "All Upfront"
          }
        }
      }
    }
  }
}`

	newBulkServer(t, bulkFixture)
	p := newBulkProvider(t)
	ctx := context.Background()

	noPrices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_1yr no-upfront): %v", err)
	}
	if len(noPrices) == 0 {
		t.Fatal("GetComputePrice(reserved_1yr): expected at least one price, got none")
	}

	partialPrices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved1YrPartial)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_1yr_partial): %v", err)
	}
	if len(partialPrices) == 0 {
		t.Fatal("GetComputePrice(reserved_1yr_partial): expected at least one price, got none")
	}

	allPrices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermReserved1YrAll)
	if err != nil {
		t.Fatalf("GetComputePrice(reserved_1yr_all): %v", err)
	}
	if len(allPrices) == 0 {
		t.Fatal("GetComputePrice(reserved_1yr_all): expected at least one price, got none")
	}

	noPrice := noPrices[0].PricePerUnit
	partialPrice := partialPrices[0].PricePerUnit
	allPrice := allPrices[0].PricePerUnit

	// Verify absolute values are in the expected ballpark.
	const tolerance = 1e-4 // wider tolerance for upfront normalisation
	if noPrice < 0.114-tolerance || noPrice > 0.114+tolerance {
		t.Errorf("no-upfront price = %v, want ~0.114", noPrice)
	}
	if partialPrice < 0.100-tolerance || partialPrice > 0.100+tolerance {
		t.Errorf("partial-upfront price = %v, want ~0.100 (0.050 + 438/8760)", partialPrice)
	}

	// Core invariant: all-upfront < partial-upfront < no-upfront.
	if allPrice >= partialPrice {
		t.Errorf("invariant violation: all-upfront (%v) must be cheaper than partial-upfront (%v)", allPrice, partialPrice)
	}
	if partialPrice >= noPrice {
		t.Errorf("invariant violation: partial-upfront (%v) must be cheaper than no-upfront (%v)", partialPrice, noPrice)
	}
}

// TestGetComputePrice_SpotTermNotSupportedInGetComputePrice verifies that
// GetComputePrice with term=spot returns an empty result (not an error).
//
// Spot pricing is NOT routed through GetComputePrice — it is handled by
// GetSpotHistory (which calls EC2 DescribeSpotPriceHistory). When a caller
// passes PricingTermSpot to GetComputePrice, skuToNormalizedPrice sees a term
// that is not OnDemand and not in _reservedTermFilters, so extractReservedPrice
// returns (0, "") and the SKU is silently skipped. The result is ([], nil).
func TestGetComputePrice_SpotTermNotSupportedInGetComputePrice(t *testing.T) {
	// A simple OnDemand fixture — even if spot matched attribute filters,
	// skuToNormalizedPrice returns nil for PricingTermSpot because spot is
	// not in _reservedTermFilters.
	const bulkFixture = `{
  "formatVersion": "aws_v1",
  "products": {
    "INVSKU4": {
      "sku": "INVSKU4",
      "productFamily": "Compute Instance",
      "attributes": {
        "instanceType":    "m5.xlarge",
        "operatingSystem": "Linux",
        "tenancy":         "Shared",
        "preInstalledSw":  "NA",
        "capacitystatus":  "Used",
        "location":        "US East (N. Virginia)"
      }
    }
  },
  "terms": {
    "OnDemand": {
      "INVSKU4": {
        "INVSKU4.ODTERM": {
          "priceDimensions": {
            "INVSKU4.ODTERM.DIM": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.1920000000"},
              "description": "$0.192 per On Demand Linux m5.xlarge Instance Hour"
            }
          },
          "termAttributes": {}
        }
      }
    },
    "Reserved": {}
  }
}`

	newBulkServer(t, bulkFixture)
	p := newBulkProvider(t)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "m5.xlarge", "us-east-1", "Linux", models.PricingTermSpot)
	if err != nil {
		// GetComputePrice must not return an error for spot — it silently
		// returns empty (spot is handled separately via GetSpotHistory).
		t.Fatalf("GetComputePrice(spot): unexpected error: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("GetComputePrice(spot): expected empty result (spot is not routed through GetComputePrice), got %d prices", len(prices))
	}
}

// --------------------------------------------------------------------------
// TestPricingResult_AuthGating (provider-contract)
// --------------------------------------------------------------------------

// TestPricingResult_AuthGating verifies that PricingResult JSON serialization
// omits contracted_prices and effective_price when auth_available is false,
// and includes them when auth_available is true.
//
// Mirrors Python test_pricing_result_summary_with_no_auth /
// test_pricing_result_summary_with_auth from test_provider_contract.py.
func TestPricingResult_AuthGating(t *testing.T) {
	now := time.Now()
	basePrice := models.NormalizedPrice{
		Provider:      models.CloudProviderAWS,
		Service:       "compute",
		SKUID:         "test-sku",
		ProductFamily: "Compute Instance",
		Description:   "m5.xlarge Linux on-demand",
		Region:        "us-east-1",
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  0.192,
		Unit:          models.PriceUnitPerHour,
		Currency:      "USD",
		FetchedAt:     &now,
	}
	contractedPrice := models.NormalizedPrice{
		Provider:      models.CloudProviderAWS,
		Service:       "compute",
		SKUID:         "test-sku-sp",
		ProductFamily: "Compute Instance",
		Description:   "m5.xlarge Compute SP",
		Region:        "us-east-1",
		PricingTerm:   models.PricingTermComputeSP,
		PricePerUnit:  0.140,
		Unit:          models.PriceUnitPerHour,
		Currency:      "USD",
		FetchedAt:     &now,
	}
	effectivePrice := &models.EffectivePrice{
		BasePrice:             basePrice,
		EffectivePricePerUnit: 0.140,
		DiscountType:          "SP",
		DiscountPct:           27.1,
		CommitmentTerm:        "1yr",
		Source:                "cost_explorer",
	}

	t.Run("no_auth_omits_contracted_and_effective", func(t *testing.T) {
		result := &models.PricingResult{
			PublicPrices:  []models.NormalizedPrice{basePrice},
			AuthAvailable: false,
			Source:        "catalog",
			SchemaVersion: "1",
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if _, ok := m["contracted_prices"]; ok {
			t.Error("contracted_prices must not be present when auth_available=false")
		}
		if _, ok := m["effective_price"]; ok {
			t.Error("effective_price must not be present when auth_available=false")
		}
		if m["auth_available"] != false {
			t.Errorf("auth_available = %v, want false", m["auth_available"])
		}
		prices, ok := m["public_prices"].([]any)
		if !ok || len(prices) == 0 {
			t.Error("public_prices must be present and non-empty")
		}
	})

	t.Run("with_auth_includes_contracted_and_effective", func(t *testing.T) {
		result := &models.PricingResult{
			PublicPrices:     []models.NormalizedPrice{basePrice},
			ContractedPrices: []models.NormalizedPrice{contractedPrice},
			EffectivePrice:   effectivePrice,
			AuthAvailable:    true,
			Source:           "catalog",
			SchemaVersion:    "1",
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if m["auth_available"] != true {
			t.Errorf("auth_available = %v, want true", m["auth_available"])
		}
		if _, ok := m["contracted_prices"]; !ok {
			t.Error("contracted_prices must be present when auth_available=true and set")
		}
		effAny, ok := m["effective_price"]
		if !ok {
			t.Fatal("effective_price must be present when auth_available=true and set")
		}
		eff, ok := effAny.(map[string]any)
		if !ok {
			t.Fatalf("effective_price is not a map: %T", effAny)
		}
		if _, ok := eff["discount_pct"]; !ok {
			t.Error("effective_price must contain discount_pct")
		}
	})
}

// --------------------------------------------------------------------------
// TestAllProvidersImplementInterface (provider-contract)
// --------------------------------------------------------------------------

// TestAllProvidersImplementInterface verifies that all three cloud pricing
// providers (AWS, Azure, GCP) satisfy the providers.Provider interface at
// runtime, and that each returns the correct canonical provider name.
//
// In Go the compile-time interface guards (var _ providers.Provider = (*P)(nil))
// in each provider package are the primary enforcement; this test provides an
// additional runtime assertion and documents the expected Name() values.
//
// Mirrors TestAllProvidersImplementInterface from test_provider_contract.py.
func TestAllProvidersImplementInterface(t *testing.T) {
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
	// AWS: use the internal struct directly (avoids credential probe in NewProvider).
	awsP := &Provider{cfg: cfg, cache: cm}
	// Azure: NewProvider requires only a cache manager and TTLs.
	azureP := azureprovider.NewProvider(cm, 24*time.Hour, 7*24*time.Hour)
	// GCP: NewProvider requires a config and cache manager.
	gcpP, err := gcpprovider.NewProvider(cfg, cm)
	if err != nil {
		t.Fatalf("gcpprovider.NewProvider: %v", err)
	}

	tests := []struct {
		name     string
		pvdr     providers.Provider
		wantName models.CloudProvider
	}{
		{"aws", awsP, models.CloudProviderAWS},
		{"azure", azureP, models.CloudProviderAzure},
		{"gcp", gcpP, models.CloudProviderGCP},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.pvdr == nil {
				t.Fatal("provider is nil")
			}
			got := tc.pvdr.Name()
			if got != tc.wantName {
				t.Errorf("Name() = %q, want %q", got, tc.wantName)
			}
			// DefaultRegion must be non-empty.
			if dr := tc.pvdr.DefaultRegion(); dr == "" {
				t.Error("DefaultRegion() must be non-empty")
			}
			// MajorRegions must be non-empty.
			if mr := tc.pvdr.MajorRegions(); len(mr) == 0 {
				t.Error("MajorRegions() must return at least one region")
			}
		})
	}
}

// TestGetElastiCachePrice_StaticFallback_404 verifies that when the bulk
// pricing endpoint returns 404 (service not in region file), the static
// fallback table is used for known node types.
func TestGetElastiCachePrice_StaticFallback_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // simulate ElastiCache bulk file not at region URL
	}))
	defer srv.Close()
	overrideBulkBaseURL(t, srv.URL)

	p := newBulkProvider(t)
	ctx := context.Background()

	for _, tc := range []struct {
		nodeType  string
		wantPrice float64
	}{
		{"cache.r7g.large", 0.166},
		{"cache.r6g.large", 0.166},
		{"cache.t4g.micro", 0.016},
	} {
		t.Run(tc.nodeType, func(t *testing.T) {
			result, err := p.GetElastiCachePrice(ctx, tc.nodeType, "us-east-1")
			if err != nil {
				t.Fatalf("GetElastiCachePrice(%q): %v", tc.nodeType, err)
			}
			if len(result) == 0 {
				t.Fatalf("GetElastiCachePrice(%q): got empty prices, want fallback price", tc.nodeType)
			}
			if result[0].PricePerUnit != tc.wantPrice {
				t.Errorf("price = %v, want %v", result[0].PricePerUnit, tc.wantPrice)
			}
			fallbackFlag := result[0].Attributes["fallback"]
			if fallbackFlag != "true" {
				t.Errorf("fallback attribute = %q, want %q", fallbackFlag, "true")
			}
		})
	}
}

// TestGetElastiCachePrice_BulkLiveURL tests the static fallback when the live
// ElastiCache bulk URL is hit. Run with -short to skip network tests.
func TestGetElastiCachePrice_BulkLive_r7gLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	// Use real bulkPricingBaseURL (no override) with bulkFallback=true.
	p := newBulkProvider(t)
	ctx := context.Background()

	result, err := p.GetElastiCachePrice(ctx, "cache.r7g.large", "us-east-1")
	if err != nil {
		t.Fatalf("GetElastiCachePrice: %v", err)
	}
	t.Logf("returned %d prices", len(result))
	for _, r := range result {
		t.Logf("  price=%v unit=%v fallback=%v desc=%s", r.PricePerUnit, r.Unit, r.Attributes["fallback"], r.Description)
	}
	if len(result) == 0 {
		t.Fatal("expected at least 1 price")
	}
}

// --------------------------------------------------------------------------
// EBS static fallback tests (Fix: bulkFallback io1/io2 avoid 449MB download)
// --------------------------------------------------------------------------

// TestGetStoragePrice_BulkFallback_io2_StaticRates verifies that in
// bulkFallback mode (no AWS credentials), GetStoragePrice("io2", ...) returns
// static hardcoded rates without touching the network, and the result includes:
//   - at least one PriceUnitPerGBMonth entry
//   - at least one PriceUnitPerIOPSMonth entry
//   - Attributes["fallback"] == "true" on all entries
func TestGetStoragePrice_BulkFallback_io2_StaticRates(t *testing.T) {
	p := newTestProvider(t)
	p.bulkFallback = true
	ctx := context.Background()

	result, err := p.GetStoragePrice(ctx, "io2", "us-west-2", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice io2 bulkFallback: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty prices for io2 static fallback")
	}

	var hasGB, hasIOPS bool
	for _, np := range result {
		if np.Unit == models.PriceUnitPerGBMonth {
			hasGB = true
			if got := np.Attributes["fallback"]; got != "true" {
				t.Errorf("io2 GB-month entry: Attributes[fallback] = %q, want \"true\"", got)
			}
			if np.PricePerUnit != 0.125 {
				t.Errorf("io2 GB rate = %v, want 0.125", np.PricePerUnit)
			}
		}
		if np.Unit == models.PriceUnitPerIOPSMonth {
			hasIOPS = true
			if got := np.Attributes["fallback"]; got != "true" {
				t.Errorf("io2 IOPS-month entry: Attributes[fallback] = %q, want \"true\"", got)
			}
			if np.PricePerUnit != 0.065 {
				t.Errorf("io2 IOPS rate = %v, want 0.065", np.PricePerUnit)
			}
		}
	}
	if !hasGB {
		t.Error("expected at least one PriceUnitPerGBMonth entry for io2")
	}
	if !hasIOPS {
		t.Error("expected at least one PriceUnitPerIOPSMonth entry for io2")
	}
}

// TestGetStoragePrice_BulkFallback_io1_StaticRates verifies the io1 static
// rates (different GB-month rate from io2).
func TestGetStoragePrice_BulkFallback_io1_StaticRates(t *testing.T) {
	p := newTestProvider(t)
	p.bulkFallback = true
	ctx := context.Background()

	result, err := p.GetStoragePrice(ctx, "io1", "us-east-1", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice io1 bulkFallback: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty prices for io1 static fallback")
	}

	var hasGB, hasIOPS bool
	for _, np := range result {
		if np.Unit == models.PriceUnitPerGBMonth {
			hasGB = true
			if np.PricePerUnit != 0.125 {
				t.Errorf("io1 GB rate = %v, want 0.125", np.PricePerUnit)
			}
		}
		if np.Unit == models.PriceUnitPerIOPSMonth {
			hasIOPS = true
			if np.PricePerUnit != 0.065 {
				t.Errorf("io1 IOPS rate = %v, want 0.065", np.PricePerUnit)
			}
		}
	}
	if !hasGB {
		t.Error("expected at least one PriceUnitPerGBMonth entry for io1")
	}
	if !hasIOPS {
		t.Error("expected at least one PriceUnitPerIOPSMonth entry for io1")
	}
}

// gp3BulkJSON is a minimal bulk-pricing fixture for a gp3 EBS volume. The
// price ($0.088/GB-month) is deliberately different from the static fallback
// rate ($0.08/GB-month) so tests can distinguish "live per-region data" from
// "static fallback" by price value alone.
const gp3BulkJSON = `{
"formatVersion": "aws_v1",
"products": {
  "GP3SKU1": {
    "sku": "GP3SKU1",
    "productFamily": "Storage",
    "attributes": {
      "volumeType": "General Purpose",
      "volumeApiName": "gp3",
      "location": "Canada (Central)"
    }
  }
},
"terms": {
  "OnDemand": {
    "GP3SKU1": {
      "GP3SKU1.JRTCKXETXF": {
        "priceDimensions": {
          "GP3SKU1.JRTCKXETXF.DIM": {
            "unit": "GB-Mo",
            "pricePerUnit": {"USD": "0.0880000000"},
            "description": "$0.088 per GB-month of General Purpose (SSD) provisioned storage"
          }
        },
        "termAttributes": {}
      }
    }
  },
  "Reserved": {}
}
}`

// gp2BulkJSON is the gp2 analogue of gp3BulkJSON, priced differently again
// ($0.105/GB-month) from both gp3's live price and gp2's static fallback
// rate ($0.10/GB-month).
const gp2BulkJSON = `{
"formatVersion": "aws_v1",
"products": {
  "GP2SKU1": {
    "sku": "GP2SKU1",
    "productFamily": "Storage",
    "attributes": {
      "volumeType": "General Purpose",
      "volumeApiName": "gp2",
      "location": "US West (Oregon)"
    }
  }
},
"terms": {
  "OnDemand": {
    "GP2SKU1": {
      "GP2SKU1.JRTCKXETXF": {
        "priceDimensions": {
          "GP2SKU1.JRTCKXETXF.DIM": {
            "unit": "GB-Mo",
            "pricePerUnit": {"USD": "0.1050000000"},
            "description": "$0.105 per GB-month of General Purpose (SSD) provisioned storage"
          }
        },
        "termAttributes": {}
      }
    }
  },
  "Reserved": {}
}
}`

// TestGetStoragePrice_BulkFallback_gp3_LivePath verifies that, in bulkFallback
// mode, GetStoragePrice("gp3", ...) now tries the live per-region bulk fetch
// first (RC3-012) instead of unconditionally returning the static rate. When
// the bulk endpoint returns a real product, the live price ($0.088) — not the
// static fallback price ($0.08) — must be returned, and the result must not
// carry Attributes["fallback"] = "true".
func TestGetStoragePrice_BulkFallback_gp3_LivePath(t *testing.T) {
	newBulkServer(t, gp3BulkJSON)
	p := newBulkProvider(t)
	ctx := context.Background()

	result, err := p.GetStoragePrice(ctx, "gp3", "ca-central-1", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice gp3 bulkFallback: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty prices for gp3 live bulk path")
	}

	var hasGB bool
	for _, np := range result {
		if np.Unit == models.PriceUnitPerGBMonth {
			hasGB = true
			if np.PricePerUnit != 0.088 {
				t.Errorf("gp3 GB rate = %v, want 0.088 (live bulk price, not the 0.08 static fallback)", np.PricePerUnit)
			}
			if got := np.Attributes["fallback"]; got == "true" {
				t.Errorf("gp3 GB-month entry: Attributes[fallback] = %q, want unset (live data)", got)
			}
		}
	}
	if !hasGB {
		t.Error("expected at least one PriceUnitPerGBMonth entry for gp3")
	}
}

// TestGetStoragePrice_BulkFallback_gp2_LivePath is the gp2 analogue of
// TestGetStoragePrice_BulkFallback_gp3_LivePath.
func TestGetStoragePrice_BulkFallback_gp2_LivePath(t *testing.T) {
	newBulkServer(t, gp2BulkJSON)
	p := newBulkProvider(t)
	ctx := context.Background()

	result, err := p.GetStoragePrice(ctx, "gp2", "us-west-2", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice gp2 bulkFallback: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty prices for gp2 live bulk path")
	}

	var hasGB bool
	for _, np := range result {
		if np.Unit == models.PriceUnitPerGBMonth {
			hasGB = true
			if np.PricePerUnit != 0.105 {
				t.Errorf("gp2 GB rate = %v, want 0.105 (live bulk price, not the 0.10 static fallback)", np.PricePerUnit)
			}
			if got := np.Attributes["fallback"]; got == "true" {
				t.Errorf("gp2 GB-month entry: Attributes[fallback] = %q, want unset (live data)", got)
			}
		}
	}
	if !hasGB {
		t.Error("expected at least one PriceUnitPerGBMonth entry for gp2")
	}
}

// TestGetStoragePrice_BulkFallback_gp3_DegradesOnFetchError verifies that
// when the live per-region bulk fetch fails (e.g. upstream 500, or a timeout
// surfaced as a transport error), GetStoragePrice("gp3", ...) gracefully
// degrades to the static published rate instead of returning an error — the
// graceful-degrade behavior called for by RC3-012 item (1).
func TestGetStoragePrice_BulkFallback_gp3_DegradesOnFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	origURL := bulkPricingBaseURL
	bulkPricingBaseURL = srv.URL
	defer func() { bulkPricingBaseURL = origURL }()

	p := newTestProvider(t)
	p.bulkFallback = true
	ctx := context.Background()

	result, err := p.GetStoragePrice(ctx, "gp3", "us-east-1", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice gp3 should degrade to static fallback on fetch error, got err: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty prices from static fallback degrade")
	}

	var hasGB bool
	for _, np := range result {
		if np.Unit == models.PriceUnitPerGBMonth {
			hasGB = true
			if np.PricePerUnit != 0.08 {
				t.Errorf("gp3 GB rate = %v, want 0.08 (static fallback)", np.PricePerUnit)
			}
			if got := np.Attributes["fallback"]; got != "true" {
				t.Errorf("gp3 GB-month entry: Attributes[fallback] = %q, want \"true\"", got)
			}
		}
	}
	if !hasGB {
		t.Error("expected at least one PriceUnitPerGBMonth entry for gp3")
	}
}

// TestGetStoragePrice_BulkFallback_CacheHit verifies that a second call for
// the same io2 region returns from cache (no re-computation needed).
func TestGetStoragePrice_BulkFallback_CacheHit(t *testing.T) {
	p := newTestProvider(t)
	p.bulkFallback = true
	ctx := context.Background()

	// First call populates cache.
	first, err := p.GetStoragePrice(ctx, "io2", "us-east-1", 0)
	if err != nil {
		t.Fatalf("first GetStoragePrice: %v", err)
	}

	// Second call should hit cache (same result).
	second, err := p.GetStoragePrice(ctx, "io2", "us-east-1", 0)
	if err != nil {
		t.Fatalf("second GetStoragePrice: %v", err)
	}
	if len(first) != len(second) {
		t.Errorf("cache hit returned %d prices, first call had %d", len(second), len(first))
	}
}
