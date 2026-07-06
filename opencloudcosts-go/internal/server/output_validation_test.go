// output_validation_test.go strengthens OutputSchema coverage beyond the
// no-provider error path exercised by TestHandlersReturnStructuredJSON: it
// wires real (mock/fake) providers into a full in-process AppServer, drives
// each priority tool's SUCCESS path through the real MCP server (in-memory
// transport, sess.CallTool), and validates the resulting JSON against the
// tool's *live* OutputSchema (as advertised by tools/list) using the same
// jsonschema package (github.com/google/jsonschema-go/jsonschema) and the
// same Resolve+Validate calls the go-sdk itself uses in mcp/server.go's
// setSchema/applySchema.
//
// This matters because these tools build their CallToolResult by hand (via
// tools.jsonText) and always return a nil structured-output value to the
// go-sdk's generic AddTool wrapper — so the go-sdk's own output-schema
// validation (which only runs when the typed handler returns a non-nil
// output value) never actually fires for any tool in this server. Without
// the tests in this file, a schema/handler drift on the success path would
// go undetected forever.
package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/server"
)

// --------------------------------------------------------------------------
// mockPricingProvider — a minimal providers.Provider implementation, wired
// into a real *server.AppServer (not the tools.Handler directly), so the
// success-path JSON we validate is exactly what a real MCP client would
// receive over the wire.
// --------------------------------------------------------------------------

type mockPricingProvider struct {
	name          string
	defaultRegion string
	majorRegions  []string
	getPriceFunc  func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error)
}

var _ server.Provider = (*mockPricingProvider)(nil)

func (m *mockPricingProvider) Name() models.CloudProvider { return models.CloudProvider(m.name) }
func (m *mockPricingProvider) DefaultRegion() string      { return m.defaultRegion }
func (m *mockPricingProvider) MajorRegions() []string     { return m.majorRegions }

func (m *mockPricingProvider) Supports(_ models.PricingDomain, _ string) bool { return true }

func (m *mockPricingProvider) SupportedTerms(_ models.PricingDomain, _ string) []models.PricingTerm {
	return []models.PricingTerm{models.PricingTermOnDemand}
}

func (m *mockPricingProvider) GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	if m.getPriceFunc != nil {
		return m.getPriceFunc(ctx, spec)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) GetComputePrice(context.Context, string, string, string, models.PricingTerm) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) GetStoragePrice(context.Context, string, string, float64) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) SearchPricing(context.Context, string, string, int) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) ListRegions(context.Context, string) ([]string, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) ListInstanceTypes(context.Context, string, string, int, float64, bool) ([]models.InstanceTypeInfo, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) CheckAvailability(context.Context, string, string, string) (bool, []string, error) {
	return false, nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) GetEffectivePrice(context.Context, models.PricingSpec) ([]models.EffectivePrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) GetSpotHistory(context.Context, models.PricingSpec, int, string) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) GetDiscountSummary(context.Context) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockPricingProvider) DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error) {
	return &models.ProviderCatalog{
		Provider: models.CloudProvider(m.name),
		Domains:  []models.PricingDomain{models.PricingDomainCompute},
	}, nil
}

func (m *mockPricingProvider) BOMAdvisories(context.Context, []string, string) ([]map[string]string, error) {
	return nil, nil
}

// --------------------------------------------------------------------------
// Fixture builders
// --------------------------------------------------------------------------

func fixtureComputePrice(provider models.CloudProvider, region, instanceType string, pricePerHour float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:     provider,
		Service:      "compute",
		SKUID:        "TEST-COMPUTE-SKU",
		Description:  instanceType + " Linux",
		Region:       region,
		Attributes:   map[string]string{"instanceType": instanceType, "vcpu": "4", "memory": "16 GiB"},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerHour,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
}

func fixtureStoragePrice(provider models.CloudProvider, region, storageType string, pricePerGBMonth float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      provider,
		Service:       "storage",
		SKUID:         "TEST-STORAGE-SKU",
		ProductFamily: "Storage",
		Description:   storageType + " storage",
		Region:        region,
		Attributes:    map[string]string{"storage_type": storageType},
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerGBMonth,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
	}
}

func fixtureStorageIOPSPrice(provider models.CloudProvider, region string, pricePerIOPSMonth float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      provider,
		Service:       "storage",
		SKUID:         "TEST-IOPS-SKU",
		ProductFamily: "Storage",
		Description:   "io2 Provisioned IOPS SSD IOPS-months",
		Region:        region,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerIOPSMonth,
		Unit:          models.PriceUnitPerIOPSMonth,
		Currency:      "USD",
	}
}

func fixtureStorageMBPSPrice(provider models.CloudProvider, region string, pricePerMBPSMonth float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      provider,
		Service:       "storage",
		SKUID:         "TEST-MBPS-SKU",
		ProductFamily: "Persistent Disk",
		Description:   "Hyperdisk Balanced provisioned throughput",
		Region:        region,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerMBPSMonth,
		Unit:          models.PriceUnitPerMBPSMonth,
		Currency:      "USD",
	}
}

func fixtureDatabasePrice(provider models.CloudProvider, region, resourceType string, pricePerHour float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:     provider,
		Service:      "rds",
		SKUID:        "TEST-DB-SKU",
		Description:  resourceType,
		Region:       region,
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerHour,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
}

// --------------------------------------------------------------------------
// jsonschema validation helpers — mirrors go-sdk mcp/server.go's
// setSchema/applySchema (Schema.Resolve + Resolved.Validate).
// --------------------------------------------------------------------------

// resolveOutputSchema parses a registered tool's live OutputSchema (as
// returned over the wire by tools/list, i.e. tool.OutputSchema holding the
// default JSON marshaling of the server's schema — a map[string]any) into a
// *jsonschema.Resolved, exactly as mcp/server.go's setSchema does for typed
// AddTool handlers.
func resolveOutputSchema(t *testing.T, tool *mcp.Tool) *jsonschema.Resolved {
	t.Helper()
	if tool.OutputSchema == nil {
		t.Fatalf("tool %q: OutputSchema is nil", tool.Name)
	}
	raw, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("tool %q: marshal OutputSchema: %v", tool.Name, err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("tool %q: unmarshal OutputSchema into jsonschema.Schema: %v", tool.Name, err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatalf("tool %q: Resolve OutputSchema: %v", tool.Name, err)
	}
	return resolved
}

// callToolSuccess invokes name via sess.CallTool, fails the test if the call
// errored, the result is flagged IsError, or the response isn't a JSON
// object, and returns the decoded payload alongside the tool's live schema.
func callToolSuccess(t *testing.T, ctx context.Context, sess *mcp.ClientSession, toolsByName map[string]*mcp.Tool, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%q): no content in response", name)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%q): content[0] is %T, not *TextContent", name, res.Content[0])
	}
	if res.IsError {
		t.Fatalf("CallTool(%q): unexpected IsError=true: %s", name, text.Text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("CallTool(%q): response is not valid JSON: %v\nresponse: %s", name, err, text.Text)
	}
	if errVal, present := payload["error"]; present {
		t.Fatalf("CallTool(%q): unexpected structured error on success-path fixture: %v\nfull response: %s", name, errVal, text.Text)
	}
	tool, ok := toolsByName[name]
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	if err := resolveOutputSchema(t, tool).Validate(payload); err != nil {
		t.Errorf("CallTool(%q): success response violates its own OutputSchema: %v\nresponse: %s", name, err, text.Text)
	}
	return payload
}

// newServerWithProviders builds a real *server.AppServer wired with provs,
// then returns an in-process, connected client session plus the live
// tool-name -> *mcp.Tool map (each tool's OutputSchema as advertised by
// tools/list — the same field validated in TestOutputSchemaParityWithSnapshot).
func newServerWithProviders(t *testing.T, provs map[string]server.Provider) (*mcp.ClientSession, map[string]*mcp.Tool) {
	t.Helper()
	cfg := &config.Config{}
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	srv := server.New(cfg, cm, provs)
	sess := connectToServer(t, srv)
	toolsByName := collectTools(t, context.Background(), sess)
	return sess, toolsByName
}

// --------------------------------------------------------------------------
// get_price — compute, and storage with/without size_gb/iops/throughput_mbps.
// --------------------------------------------------------------------------

func TestOutputSchema_GetPrice_ComputeSuccess(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", 0.192)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	callToolSuccess(t, ctx, sess, toolsByName, "get_price", map[string]any{
		"spec": map[string]any{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
	})
}

func TestOutputSchema_GetPrice_StorageWithSizeGB(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureStoragePrice(models.CloudProviderGCP, spec.GetRegion(), "gcs_standard", 0.020)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"gcp": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "get_price", map[string]any{
		"spec": map[string]any{
			"provider":      "gcp",
			"domain":        "storage",
			"resource_type": "gcs_standard",
			"region":        "us-central1",
			"size_gb":       500,
		},
	})
	prices, _ := resp["public_prices"].([]any)
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %v", resp["public_prices"])
	}
	p0, _ := prices[0].(map[string]any)
	if _, ok := p0["monthly_estimate"]; !ok {
		t.Errorf("expected monthly_estimate scaled by size_gb, got: %v", p0)
	}
}

func TestOutputSchema_GetPrice_StorageWithIOPS(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureStorageIOPSPrice(models.CloudProviderAWS, spec.GetRegion(), 0.065)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	callToolSuccess(t, ctx, sess, toolsByName, "get_price", map[string]any{
		"spec": map[string]any{
			"provider":      "aws",
			"domain":        "storage",
			"resource_type": "io2",
			"region":        "us-east-1",
			"iops":          1000,
		},
	})
}

func TestOutputSchema_GetPrice_StorageWithThroughputMBPS(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureStorageMBPSPrice(models.CloudProviderGCP, spec.GetRegion(), 0.048)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"gcp": pvdr})
	ctx := context.Background()

	callToolSuccess(t, ctx, sess, toolsByName, "get_price", map[string]any{
		"spec": map[string]any{
			"provider":        "gcp",
			"domain":          "storage",
			"resource_type":   "hyperdisk_balanced",
			"region":          "us-central1",
			"throughput_mbps": 250,
		},
	})
}

// --------------------------------------------------------------------------
// get_prices_batch
// --------------------------------------------------------------------------

func TestOutputSchema_GetPricesBatch_Success(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			cs, ok := spec.(*models.ComputePricingSpec)
			rt := "m5.xlarge"
			if ok {
				rt = cs.ResourceType
			}
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), rt, 0.192)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	callToolSuccess(t, ctx, sess, toolsByName, "get_prices_batch", map[string]any{
		"provider":       "aws",
		"instance_types": []string{"m5.xlarge", "c5.xlarge"},
		"region":         "us-east-1",
	})
}

// --------------------------------------------------------------------------
// compare_prices
// --------------------------------------------------------------------------

func TestOutputSchema_ComparePrices_Success(t *testing.T) {
	regionPrices := map[string]float64{
		"us-east-1": 0.192,
		"eu-west-1": 0.210,
	}
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			p, ok := regionPrices[spec.GetRegion()]
			if !ok {
				return &models.PricingResult{Source: "catalog", SchemaVersion: "1"}, nil
			}
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", p)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	callToolSuccess(t, ctx, sess, toolsByName, "compare_prices", map[string]any{
		"spec": map[string]any{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
		},
		"regions":         []string{"us-east-1", "eu-west-1"},
		"baseline_region": "us-east-1",
	})
}

// --------------------------------------------------------------------------
// get_price_by_sku — requires a real *awsprovider.Provider (HandleGetPriceBySKU
// type-asserts the resolved provider to *awsprovider.Provider), with the bulk
// pricing endpoint mocked out via awsprovider.SetBulkPricingBaseURLForTesting,
// mirroring internal/tools/sku_lookup_test.go's happy-path fixture.
// --------------------------------------------------------------------------

func skuFixtureJSON(sku, usageType, location, priceUSD string) string {
	return `{"products":{"` + sku + `":{"sku":"` + sku + `","productFamily":"Compute Instance","attributes":{"usagetype":"` + usageType + `","instanceType":"r6id.24xlarge","location":"` + location + `","operatingSystem":"Linux","tenancy":"Shared","capacitystatus":"Used"}}},` +
		`"terms":{"OnDemand":{"` + sku + `":{"` + sku + `.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"` + sku + `.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"` + priceUSD + `"}}}}}},"Reserved":{}}}`
}

func TestOutputSchema_GetPriceBySKU_Success(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")))
	})
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(httpSrv.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}

	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": realAWS})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "get_price_by_sku", map[string]any{
		"sku":     "BoxUsage:r6id.24xlarge",
		"service": "AmazonEC2",
		"regions": []string{"us-east-1"},
	})
	if resp["usage_type_suffix"] != "BoxUsage:r6id.24xlarge" {
		t.Errorf("usage_type_suffix: got %v, want BoxUsage:r6id.24xlarge", resp["usage_type_suffix"])
	}
}

// --------------------------------------------------------------------------
// estimate_bom — mixed compute + database + storage line items.
// --------------------------------------------------------------------------

func TestOutputSchema_EstimateBOM_Success(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			var price models.NormalizedPrice
			switch s := spec.(type) {
			case *models.ComputePricingSpec:
				price = fixtureComputePrice(models.CloudProviderAWS, s.GetRegion(), s.ResourceType, 0.192)
			case *models.DatabasePricingSpec:
				price = fixtureDatabasePrice(models.CloudProviderAWS, s.GetRegion(), s.ResourceType, 0.016)
			case *models.StoragePricingSpec:
				price = fixtureStoragePrice(models.CloudProviderAWS, s.GetRegion(), s.StorageType, 0.08)
			default:
				return nil, providers.ErrNotSupported
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}, Source: "catalog", SchemaVersion: "1"}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "estimate_bom", map[string]any{
		"items": []any{
			map[string]any{
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"region":        "us-east-1",
				"quantity":      2,
			},
			map[string]any{
				"provider":      "aws",
				"domain":        "database",
				"service":       "rds",
				"resource_type": "db.t4g.micro",
				"engine":        "MySQL",
				"deployment":    "single-az",
				"region":        "us-east-1",
			},
			map[string]any{
				"provider":     "aws",
				"domain":       "storage",
				"storage_type": "gp3",
				"region":       "us-east-1",
				"size_gb":      100,
			},
		},
	})
	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 3 {
		t.Fatalf("expected 3 line_items, got %v", resp["line_items"])
	}
}

// --------------------------------------------------------------------------
// compare_bom — two providers, one workload item.
// --------------------------------------------------------------------------

func TestOutputSchema_CompareBOM_Success(t *testing.T) {
	awsPvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", 0.192)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	gcpPvdr := &mockPricingProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderGCP, spec.GetRegion(), "n2-standard-4", 0.170)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": awsPvdr, "gcp": gcpPvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "compare_bom", map[string]any{
		"providers": []string{"aws", "gcp"},
		"workload": map[string]any{
			"web_server": map[string]any{
				"type":      "compute",
				"vcpus":     4,
				"memory_gb": 16,
				"quantity":  1,
			},
		},
		"terms": []string{"on_demand"},
	})
	cmp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("expected comparison map, got %T", resp["comparison"])
	}
	for _, prov := range []string{"aws", "gcp"} {
		if _, ok := cmp[prov]; !ok {
			t.Errorf("expected provider %q in comparison, got %v", prov, cmp)
		}
	}
}

// --------------------------------------------------------------------------
// compare_bom_regions — AWS-only, cheapest-first with baseline delta.
// --------------------------------------------------------------------------

func TestOutputSchema_CompareBOMRegions_Success(t *testing.T) {
	regionPrices := map[string]float64{
		"us-east-1": 0.192,
		"us-west-2": 0.150,
		"eu-west-1": 0.210,
	}
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			p, ok := regionPrices[spec.GetRegion()]
			if !ok {
				return nil, providers.ErrNotSupported
			}
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", p)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "compare_bom_regions", map[string]any{
		"items": []any{
			map[string]any{
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"quantity":      1,
			},
		},
		"regions":         []string{"us-east-1", "us-west-2", "eu-west-1"},
		"baseline_region": "us-east-1",
	})
	if _, ok := resp["regions"]; !ok {
		t.Errorf("expected top-level 'regions' key, got: %v", resp)
	}
}

// --------------------------------------------------------------------------
// estimate_unit_economics — same items shape as estimate_bom, plus a
// units-per-month divisor producing cost_per_unit.
// --------------------------------------------------------------------------

func TestOutputSchema_EstimateUnitEconomics_Success(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "t3.small", 0.10)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "estimate_unit_economics", map[string]any{
		"items": []any{
			map[string]any{
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "t3.small",
				"region":        "us-east-1",
			},
		},
		"units_per_month": 10000,
		"unit_label":      "user",
	})
	if _, ok := resp["cost_per_unit"].(map[string]any); !ok {
		t.Errorf("expected cost_per_unit map, got %v", resp["cost_per_unit"])
	}
	if _, ok := resp["infrastructure_monthly"].(map[string]any); !ok {
		t.Errorf("expected infrastructure_monthly map, got %v", resp["infrastructure_monthly"])
	}
}

// --------------------------------------------------------------------------
// find_cheapest_region — region fan-out, sorted cheapest-first.
// --------------------------------------------------------------------------

func TestOutputSchema_FindCheapestRegion_Success(t *testing.T) {
	regionPrices := map[string]float64{
		"us-east-1": 0.192,
		"us-west-2": 0.150,
		"eu-west-1": 0.210,
	}
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		majorRegions:  []string{"us-east-1", "us-west-2", "eu-west-1"},
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			p, ok := regionPrices[spec.GetRegion()]
			if !ok {
				return nil, providers.ErrNotSupported
			}
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", p)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "find_cheapest_region", map[string]any{
		"spec": map[string]any{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
		"regions": []string{"us-east-1", "us-west-2", "eu-west-1"},
	})
	if resp["cheapest_region"] != "us-west-2" {
		t.Errorf("cheapest_region: got %v, want us-west-2", resp["cheapest_region"])
	}
	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(sorted) != 3 {
		t.Fatalf("all_regions_sorted: expected 3 entries, got %v", resp["all_regions_sorted"])
	}
}

// --------------------------------------------------------------------------
// find_available_regions — region fan-out, availability partitioning.
// --------------------------------------------------------------------------

func TestOutputSchema_FindAvailableRegions_Success(t *testing.T) {
	availableRegions := map[string]bool{
		"us-east-1": true,
		"us-west-2": true,
	}
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		majorRegions:  []string{"us-east-1", "us-west-2", "eu-west-1"},
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if !availableRegions[spec.GetRegion()] {
				return nil, providers.ErrNotSupported
			}
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", 0.192)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "find_available_regions", map[string]any{
		"spec": map[string]any{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
		"regions": []string{"us-east-1", "us-west-2", "eu-west-1"},
	})
	availableIn, ok := resp["available_in"].(float64)
	if !ok || int(availableIn) != 2 {
		t.Errorf("expected available_in=2, got %v", resp["available_in"])
	}
	sorted, ok := resp["regions_sorted_cheapest_first"].([]any)
	if !ok || len(sorted) != 2 {
		t.Fatalf("regions_sorted_cheapest_first: expected 2 entries, got %v", resp["regions_sorted_cheapest_first"])
	}
	notAvail, ok := resp["not_available_in"].([]any)
	if !ok || len(notAvail) != 1 || notAvail[0] != "eu-west-1" {
		t.Errorf("expected not_available_in=[eu-west-1], got %v", resp["not_available_in"])
	}
}

// --------------------------------------------------------------------------
// Negative control — guards the guard.
//
// All 13 success tests above share resolveOutputSchema/callToolSuccess. A
// silent regression in that shared validation path (e.g. a future refactor
// that stops calling Validate, or swallows its error) would make every one
// of those tests pass without actually checking anything, and nothing would
// notice. This test proves the mechanism still rejects a bad payload,
// including at a NESTED path (not just the top level), so a future reader
// doesn't have to wonder whether the success tests are vacuous.
// --------------------------------------------------------------------------

func TestOutputSchema_NegativeControl_RejectsNestedTypeMismatch(t *testing.T) {
	pvdr := &mockPricingProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{fixtureComputePrice(models.CloudProviderAWS, spec.GetRegion(), "m5.xlarge", 0.192)},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	sess, toolsByName := newServerWithProviders(t, map[string]server.Provider{"aws": pvdr})
	ctx := context.Background()

	resp := callToolSuccess(t, ctx, sess, toolsByName, "estimate_bom", map[string]any{
		"items": []any{
			map[string]any{
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"region":        "us-east-1",
				"quantity":      1,
			},
		},
	})

	totals, ok := resp["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals missing or wrong shape: %#v", resp["totals"])
	}
	monthly, ok := totals["monthly"].(map[string]any)
	if !ok {
		t.Fatalf("totals.monthly missing or wrong shape: %#v", totals["monthly"])
	}

	// Corrupt a NESTED field: totals.monthly.amount must be a number per
	// schemaEstimateBOMOutput. Validate directly (bypassing callToolSuccess,
	// which would fail the test) to confirm the schema genuinely rejects it.
	monthly["amount"] = "not-a-number-should-fail"

	tool := toolsByName["estimate_bom"]
	if err := resolveOutputSchema(t, tool).Validate(resp); err == nil {
		t.Fatal("expected validation to fail on corrupted nested totals.monthly.amount, but it passed — " +
			"the output-schema validation harness may be vacuous")
	}
}
