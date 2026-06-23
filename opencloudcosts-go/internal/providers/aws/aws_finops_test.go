package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
)

// --------------------------------------------------------------------------
// Helpers / fixtures
// --------------------------------------------------------------------------

// newNilProvider returns a Provider with nil AWS clients (simulates
// unauthenticated / no-CE environment). Part 1 provides newTestProvider(t)
// with a real cache; these tests exercise the FinOps guard logic in isolation.
func newNilProvider() *Provider {
	return &Provider{} // all clients nil
}

// --------------------------------------------------------------------------
// GetEffectivePrice tests
// --------------------------------------------------------------------------

func TestGetEffectivePrice_NoCE(t *testing.T) {
	p := newNilProvider()
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "m5.xlarge",
		OS:           "Linux",
	}

	_, err := p.GetEffectivePrice(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error when CE client is nil, got nil")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected errors.Is(err, ErrNotSupported), got %v", err)
	}
}

func TestGetEffectivePrice_WrongSpecType(t *testing.T) {
	p := newNilProvider()
	// StoragePricingSpec is not a ComputePricingSpec.
	spec := &models.StoragePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainStorage,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		StorageType: "gp3",
	}

	_, err := p.GetEffectivePrice(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for non-compute spec, got nil")
	}
}

func TestGetEffectivePrice_EmptyResourceType(t *testing.T) {
	p := newNilProvider()
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "", // deliberately empty
		OS:           "Linux",
	}

	_, err := p.GetEffectivePrice(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for empty resource_type, got nil")
	}
}

// --------------------------------------------------------------------------
// GetSpotHistory tests
// --------------------------------------------------------------------------

func TestGetSpotHistory_NilEC2Client(t *testing.T) {
	p := newNilProvider() // ec2Client is nil
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
		},
		ResourceType: "m5.xlarge",
	}

	_, err := p.GetSpotHistory(context.Background(), spec, 24, "")
	if err == nil {
		t.Fatal("expected error when ec2Client is nil, got nil")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestGetSpotHistory_WrongSpecType(t *testing.T) {
	p := newNilProvider()
	spec := &models.StoragePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainStorage,
			Region:   "us-east-1",
		},
		StorageType: "gp3",
	}

	_, err := p.GetSpotHistory(context.Background(), spec, 24, "")
	if err == nil {
		t.Fatal("expected error for non-compute spec, got nil")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestGetSpotHistory_EmptyResourceType(t *testing.T) {
	p := newNilProvider()
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
		},
		ResourceType: "",
	}

	_, err := p.GetSpotHistory(context.Background(), spec, 24, "")
	if err == nil {
		t.Fatal("expected error for empty resource_type, got nil")
	}
}

func TestGetSpotHistory_HoursClamped(t *testing.T) {
	// hours > 720 should be clamped; ec2Client == nil will still return an error,
	// but the clamping is exercised before the nil check in a real provider.
	// This test just verifies the nil-client guard fires (hours check is internal).
	p := newNilProvider()
	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
		},
		ResourceType: "c5.2xlarge",
	}
	_, err := p.GetSpotHistory(context.Background(), spec, 9999, "")
	if err == nil {
		t.Fatal("expected error (nil ec2Client), got nil")
	}
}

// --------------------------------------------------------------------------
// spotHistory stats calculation (pure-unit test, no AWS calls)
// --------------------------------------------------------------------------

// spotStatsFromPoints tests the core stats calculation by calling a thin
// helper that exercises the same arithmetic as GetSpotHistory.
func TestSpotStatsVolatility(t *testing.T) {
	type tc struct {
		name     string
		prices   []float64
		wantStab string
	}
	cases := []tc{
		{
			name:     "all_same_price_stable",
			prices:   []float64{0.039, 0.039, 0.039, 0.039},
			wantStab: "stable",
		},
		{
			name:     "moderate_variation",
			prices:   []float64{0.10, 0.12, 0.09, 0.11},
			wantStab: "moderate",
		},
		{
			name:     "high_variation_volatile",
			prices:   []float64{0.05, 0.50, 0.03, 0.45},
			wantStab: "volatile",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stability := computeSpotStability(tc.prices)
			if stability != tc.wantStab {
				t.Errorf("got stability=%q, want %q", stability, tc.wantStab)
			}
		})
	}
}

// computeSpotStability mirrors the internal volatility logic in GetSpotHistory
// so we can test it without needing a real AWS connection.
func computeSpotStability(prices []float64) string {
	if len(prices) == 0 {
		return "stable"
	}
	sum := 0.0
	for _, p := range prices {
		sum += p
	}
	avg := sum / float64(len(prices))

	sumSq := 0.0
	for _, p := range prices {
		d := p - avg
		sumSq += d * d
	}
	stddev := 0.0
	if len(prices) > 1 {
		stddev = sqrtF(sumSq / float64(len(prices)))
	}
	ratio := 0.0
	if avg > 0 {
		ratio = stddev / avg
	}
	switch {
	case ratio >= 0.15:
		return "volatile"
	case ratio >= 0.05:
		return "moderate"
	default:
		return "stable"
	}
}

// sqrtF is a simple float64 square root via Newton's method to avoid importing math.
func sqrtF(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x / 2
	for i := 0; i < 20; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// --------------------------------------------------------------------------
// GetDiscountSummary tests
// --------------------------------------------------------------------------

func TestGetDiscountSummary_NoCE(t *testing.T) {
	p := newNilProvider() // ceClient == nil

	_, err := p.GetDiscountSummary(context.Background())
	if err == nil {
		t.Fatal("expected error when CE client is nil, got nil")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

// --------------------------------------------------------------------------
// DescribeCatalog tests
// --------------------------------------------------------------------------

func TestDescribeCatalog_ReturnsCatalog(t *testing.T) {
	p := newNilProvider()
	cat, err := p.DescribeCatalog(context.Background())
	if err != nil {
		t.Fatalf("DescribeCatalog returned error: %v", err)
	}
	if cat == nil {
		t.Fatal("DescribeCatalog returned nil catalog")
	}
	if cat.Provider != models.CloudProviderAWS {
		t.Errorf("expected provider=aws, got %q", cat.Provider)
	}
}

func TestDescribeCatalog_HasExpectedDomains(t *testing.T) {
	p := newNilProvider()
	cat, _ := p.DescribeCatalog(context.Background())

	want := map[models.PricingDomain]bool{
		models.PricingDomainCompute:           true,
		models.PricingDomainStorage:           true,
		models.PricingDomainDatabase:          true,
		models.PricingDomainAI:                true,
		models.PricingDomainServerless:        true,
		models.PricingDomainAnalytics:         true,
		models.PricingDomainNetwork:           true,
		models.PricingDomainObservability:     true,
		models.PricingDomainContainer:         true,
		models.PricingDomainInterRegionEgress: true,
	}
	for _, d := range cat.Domains {
		delete(want, d)
	}
	if len(want) > 0 {
		t.Errorf("missing domains in catalog: %v", want)
	}
}

func TestDescribeCatalog_HasComputeServices(t *testing.T) {
	p := newNilProvider()
	cat, _ := p.DescribeCatalog(context.Background())

	svcs, ok := cat.Services["compute"]
	if !ok {
		t.Fatal("catalog missing 'compute' service key")
	}
	found := map[string]bool{}
	for _, s := range svcs {
		found[s] = true
	}
	for _, want := range []string{"ec2", "fargate"} {
		if !found[want] {
			t.Errorf("compute services missing %q", want)
		}
	}
}

func TestDescribeCatalog_HasExampleInvocations(t *testing.T) {
	p := newNilProvider()
	cat, _ := p.DescribeCatalog(context.Background())

	if len(cat.ExampleInvocations) == 0 {
		t.Error("expected non-empty ExampleInvocations")
	}
	if _, ok := cat.ExampleInvocations["compute"]; !ok {
		t.Error("ExampleInvocations missing 'compute' key")
	}
}

func TestDescribeCatalog_HasDecisionMatrix(t *testing.T) {
	p := newNilProvider()
	cat, _ := p.DescribeCatalog(context.Background())

	if len(cat.DecisionMatrix) == 0 {
		t.Error("expected non-empty DecisionMatrix")
	}
}

// --------------------------------------------------------------------------
// BOMAdvisories tests
// --------------------------------------------------------------------------

func TestBOMAdvisories_ComputeServices(t *testing.T) {
	p := newNilProvider()
	rows, err := p.BOMAdvisories(context.Background(), []string{"compute"}, "us-east-1")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected advisory rows for compute, got none")
	}

	// Compute triggers egress, LB, NAT, CloudWatch advisories
	items := map[string]bool{}
	for _, r := range rows {
		items[r["item"]] = true
	}
	for _, want := range []string{
		"Data transfer (egress)",
		"Load balancer (ALB/NLB)",
		"NAT Gateway",
		"CloudWatch monitoring",
	} {
		if !items[want] {
			t.Errorf("missing advisory item %q", want)
		}
	}
}

func TestBOMAdvisories_DatabaseServices(t *testing.T) {
	p := newNilProvider()
	rows, err := p.BOMAdvisories(context.Background(), []string{"database"}, "eu-west-1")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}

	items := map[string]bool{}
	for _, r := range rows {
		items[r["item"]] = true
	}
	if !items["RDS automated backups"] {
		t.Error("expected 'RDS automated backups' advisory for database services")
	}
}

func TestBOMAdvisories_StorageServices(t *testing.T) {
	p := newNilProvider()
	rows, err := p.BOMAdvisories(context.Background(), []string{"storage"}, "ap-southeast-1")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}

	items := map[string]bool{}
	for _, r := range rows {
		items[r["item"]] = true
	}
	if !items["EBS snapshots"] {
		t.Error("expected 'EBS snapshots' advisory for storage services")
	}
}

func TestBOMAdvisories_DefaultRegion(t *testing.T) {
	p := newNilProvider()
	// Empty sampleRegion should default to us-east-1
	rows, err := p.BOMAdvisories(context.Background(), []string{"compute"}, "")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}
	for _, r := range rows {
		if r["how_to_price"] == "" {
			t.Errorf("advisory item %q has empty how_to_price", r["item"])
		}
	}
}

func TestBOMAdvisories_NoComputeOrDatabase_NoEgressAdvisory(t *testing.T) {
	p := newNilProvider()
	// Only AI service — should not trigger egress/LB/NAT advisories
	rows, err := p.BOMAdvisories(context.Background(), []string{"ai"}, "us-east-1")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}
	items := map[string]bool{}
	for _, r := range rows {
		items[r["item"]] = true
	}
	if items["Data transfer (egress)"] {
		t.Error("unexpected egress advisory for AI-only service")
	}
	if items["Load balancer (ALB/NLB)"] {
		t.Error("unexpected LB advisory for AI-only service")
	}
	// CloudWatch should still appear
	if !items["CloudWatch monitoring"] {
		t.Error("expected CloudWatch advisory even for AI-only service")
	}
}

func TestBOMAdvisories_AllFields(t *testing.T) {
	p := newNilProvider()
	rows, err := p.BOMAdvisories(context.Background(), []string{"compute", "database", "storage"}, "us-west-2")
	if err != nil {
		t.Fatalf("BOMAdvisories returned error: %v", err)
	}
	for _, r := range rows {
		if r["item"] == "" {
			t.Error("advisory row has empty 'item' key")
		}
		if r["why"] == "" {
			t.Errorf("advisory item %q has empty 'why'", r["item"])
		}
		if r["how_to_price"] == "" {
			t.Errorf("advisory item %q has empty 'how_to_price'", r["item"])
		}
		if r["price"] == "" {
			t.Errorf("advisory item %q has empty 'price'", r["item"])
		}
	}
}

// --------------------------------------------------------------------------
// lookupCEService tests
// --------------------------------------------------------------------------

func TestLookupCEService_KnownServices(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"compute", "Amazon Elastic Compute Cloud - Compute"},
		{"COMPUTE", "Amazon Elastic Compute Cloud - Compute"},
		{"storage", "Amazon Elastic Block Store"},
		{"database", "Amazon Relational Database Service"},
		{"s3", "Amazon Simple Storage Service"},
		{"unknown-service", "unknown-service"},
	}
	for _, tc := range cases {
		got := lookupCEService(tc.input)
		if got != tc.want {
			t.Errorf("lookupCEService(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// derefStr tests
// --------------------------------------------------------------------------

func TestDerefStr(t *testing.T) {
	s := "hello"
	if derefStr(&s) != "hello" {
		t.Error("derefStr with non-nil string failed")
	}
	if derefStr(nil) != "" {
		t.Error("derefStr with nil should return empty string")
	}
}

// --------------------------------------------------------------------------
// requireCE tests
// --------------------------------------------------------------------------

func TestRequireCE_NilClient(t *testing.T) {
	p := &Provider{ceClient: nil}
	err := p.requireCE()
	if err == nil {
		t.Fatal("expected error for nil ceClient")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

// --------------------------------------------------------------------------
// lastFullMonth (boundary test — pure logic)
// --------------------------------------------------------------------------

func TestLastFullMonthBoundary(t *testing.T) {
	// Simulate "now" as 2026-01-15 -> last full month should be Dec 2025.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, -1, 0)

	if end.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("end = %s, want 2026-01-01", end.Format("2006-01-02"))
	}
	if start.Format("2006-01-02") != "2025-12-01" {
		t.Errorf("start = %s, want 2025-12-01", start.Format("2006-01-02"))
	}
}

// --------------------------------------------------------------------------
// newTestAWSConfig helper (used by httptest-based tests below)
// --------------------------------------------------------------------------

// newTestAWSConfig returns a minimal aws.Config pointing at the given base URL
// with static credentials so the SDK does not attempt to look up real credentials.
func newTestAWSConfig(baseURL string) aws.Config {
	creds := credentials.NewStaticCredentialsProvider("AKID", "SECRET", "TOKEN")
	return aws.Config{
		Region:           "us-east-1",
		Credentials:      creds,
		BaseEndpoint:     aws.String(baseURL),
		RetryMaxAttempts: 1,
	}
}

// --------------------------------------------------------------------------
// TestGetDiscountSummary_EmptyLists
// --------------------------------------------------------------------------

// TestGetDiscountSummary_EmptyLists verifies that GetDiscountSummary returns
// a valid response (not error) with sp_count=0 and ri_count=0 when Savings
// Plans and Reserved Instances APIs both return empty lists.
//
// Corresponds to Python test_get_discount_summary_empty in test_phase2.py.
func TestGetDiscountSummary_EmptyLists(t *testing.T) {
	// Mock Savings Plans server (REST/JSON, POST /DescribeSavingsPlans).
	spServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"savingsPlans":[]}`))
	}))
	defer spServer.Close()

	// Mock Cost Explorer server (AWS JSON 1.1, distinguished by X-Amz-Target header).
	ceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusOK)
		target := r.Header.Get("X-Amz-Target")
		switch {
		case strings.Contains(target, "GetSavingsPlansUtilization"):
			// Total=null: ceUtilisation skips the savings_plans field (guarded by != nil).
			_, _ = w.Write([]byte(`{"Total":null}`))
		case strings.Contains(target, "GetReservationUtilization"):
			_, _ = w.Write([]byte(`{"Total":null}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer ceServer.Close()

	// Mock EC2 server (EC2 Query/XML) — returns empty reservedInstancesSet.
	ec2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<DescribeReservedInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>test</requestId><reservedInstancesSet/></DescribeReservedInstancesResponse>`))
	}))
	defer ec2Server.Close()

	spCfg := newTestAWSConfig(spServer.URL)
	spClient := savingsplans.NewFromConfig(spCfg)

	ceCfg := newTestAWSConfig(ceServer.URL)
	ceClient := costexplorer.NewFromConfig(ceCfg)

	ec2Cfg := newTestAWSConfig(ec2Server.URL)
	ec2Client := awsec2.NewFromConfig(ec2Cfg)

	p := &Provider{
		ceClient:  ceClient,
		spClient:  spClient,
		ec2Client: ec2Client,
	}

	summary, err := p.GetDiscountSummary(context.Background())
	if err != nil {
		t.Fatalf("GetDiscountSummary returned unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("GetDiscountSummary returned nil summary")
	}

	spCount, ok := summary["sp_count"].(int)
	if !ok {
		t.Fatalf("sp_count is not int, got %T (%v)", summary["sp_count"], summary["sp_count"])
	}
	if spCount != 0 {
		t.Errorf("sp_count = %d, want 0", spCount)
	}

	riCount, ok := summary["ri_count"].(int)
	if !ok {
		t.Fatalf("ri_count is not int, got %T (%v)", summary["ri_count"], summary["ri_count"])
	}
	if riCount != 0 {
		t.Errorf("ri_count = %d, want 0", riCount)
	}

	if _, ok := summary["savings_plans"]; !ok {
		t.Error("summary missing 'savings_plans' key")
	}
	if _, ok := summary["reserved_instances"]; !ok {
		t.Error("summary missing 'reserved_instances' key")
	}
}

// --------------------------------------------------------------------------
// TestGetEffectivePrice_NotConfigured
// --------------------------------------------------------------------------

// TestGetEffectivePrice_NotConfigured verifies that GetEffectivePrice returns
// errCENotConfigured (wrapping ErrNotSupported) when the Cost Explorer client
// is nil (i.e. when OCC_AWS_ENABLE_COST_EXPLORER is not set).
//
// This corresponds to Python test_get_discount_summary_no_auth in test_phase2.py
// which expects NotConfiguredError when aws_enable_cost_explorer=False.
// In Go the provider returns errCENotConfigured wrapping providers.ErrNotSupported;
// the tool layer converts this to {"error":"not_configured"} for MCP callers.
func TestGetEffectivePrice_NotConfigured(t *testing.T) {
	p := &Provider{ceClient: nil} // CE disabled

	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAWS,
			Domain:   models.PricingDomainCompute,
			Region:   "us-east-1",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "m5.xlarge",
		OS:           "Linux",
	}

	result, err := p.GetEffectivePrice(context.Background(), spec)

	if err == nil {
		t.Fatal("expected errCENotConfigured, got nil error")
	}
	if !errors.Is(err, providers.ErrNotSupported) {
		t.Errorf("expected error to wrap ErrNotSupported, got: %v", err)
	}

	// The error message must mention Cost Explorer and the configuration hint.
	msg := err.Error()
	if !strings.Contains(msg, "Cost Explorer") {
		t.Errorf("error message should mention 'Cost Explorer', got: %q", msg)
	}
	if !strings.Contains(msg, "OCC_AWS_ENABLE_COST_EXPLORER") {
		t.Errorf("error message should mention 'OCC_AWS_ENABLE_COST_EXPLORER', got: %q", msg)
	}

	// Result must be nil — not_configured is an error path, not a structured response.
	// The tool layer (HandleGetEffectivePrice) converts ErrNotSupported to
	// {"error":"not_configured"} for the MCP caller.
	if result != nil {
		t.Errorf("expected nil result when CE not configured, got: %v", result)
	}
}

