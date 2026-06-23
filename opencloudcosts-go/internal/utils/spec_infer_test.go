package utils

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// FillDomain — service-keyed lookup
// ---------------------------------------------------------------------------

var serviceToExpectedDomain = []struct {
	service string
	domain  string
}{
	{"rds", "database"},
	{"cloud_sql", "database"},
	{"elasticache", "database"},
	{"memorystore", "database"},
	{"sql", "database"},
	{"cosmos", "database"},
	{"bigquery", "analytics"},
	{"cloud_nat", "network"},
	{"cloud_lb", "network"},
	{"cloud_cdn", "network"},
	{"nat", "network"},
	{"lb", "network"},
	{"cdn", "network"},
	{"cloud_armor", "observability"},
	{"cloudwatch", "observability"},
	{"cloud_monitoring", "observability"},
	{"bedrock", "ai"},
	{"gemini", "ai"},
	{"vertex", "ai"},
	{"openai", "ai"},
	{"sagemaker", "ai"},
	{"lambda", "serverless"},
	{"functions", "serverless"},
	{"azure_functions", "serverless"},
	{"cloud_functions", "serverless"},
	{"cloud_run", "serverless"},
	{"gke", "container"},
	{"eks", "container"},
	{"aks", "container"},
	{"data_transfer", "inter_region_egress"},
	{"egress", "inter_region_egress"},
}

func TestFillDomainFromService(t *testing.T) {
	for _, tc := range serviceToExpectedDomain {
		tc := tc
		t.Run(tc.service, func(t *testing.T) {
			spec := map[string]interface{}{"service": tc.service, "provider": "aws"}
			result := FillDomain(spec)
			if result["domain"] != tc.domain {
				t.Errorf("service=%q: got domain %q want %q", tc.service, result["domain"], tc.domain)
			}
		})
	}
}

func TestFillDomainServiceCaseInsensitive(t *testing.T) {
	spec := map[string]interface{}{"service": "RDS", "provider": "aws"}
	result := FillDomain(spec)
	if result["domain"] != "database" {
		t.Errorf("got %q want database", result["domain"])
	}
}

func TestFillDomainDoesNotMutateOriginalSpec(t *testing.T) {
	spec := map[string]interface{}{"service": "rds", "provider": "aws"}
	result := FillDomain(spec)
	if _, ok := spec["domain"]; ok {
		t.Error("FillDomain must not mutate the original spec")
	}
	if result["domain"] != "database" {
		t.Errorf("got %q want database", result["domain"])
	}
}

// ---------------------------------------------------------------------------
// FillDomain — domain already present (no-op, same map returned)
// ---------------------------------------------------------------------------

func TestFillDomainPreservesExistingDomain(t *testing.T) {
	spec := map[string]interface{}{"service": "rds", "domain": "compute", "provider": "aws"}
	result := FillDomain(spec)
	if result["domain"] != "compute" {
		t.Errorf("existing domain must be preserved, got %q", result["domain"])
	}
	// Must return the same map (identity check via pointer comparison)
	specPtr := fmt.Sprintf("%p", (interface{})(spec))
	resultPtr := fmt.Sprintf("%p", (interface{})(result))
	if specPtr != resultPtr {
		t.Errorf("expected same map (identity); spec=%s result=%s", specPtr, resultPtr)
	}
}

// ---------------------------------------------------------------------------
// FillDomain — storage_type fallback
// ---------------------------------------------------------------------------

func TestFillDomainStorageTypeInfersStorage(t *testing.T) {
	spec := map[string]interface{}{"storage_type": "gp3", "provider": "aws"}
	result := FillDomain(spec)
	if result["domain"] != "storage" {
		t.Errorf("got %q want storage", result["domain"])
	}
}

func TestFillDomainStorageTypeTakesPrecedenceOverUnknownService(t *testing.T) {
	spec := map[string]interface{}{"service": "unknown_svc", "storage_type": "ssd", "provider": "aws"}
	result := FillDomain(spec)
	if result["domain"] != "storage" {
		t.Errorf("got %q want storage", result["domain"])
	}
}

// ---------------------------------------------------------------------------
// FillDomain — resource_type patterns
// ---------------------------------------------------------------------------

func TestFillDomainDBResourceTypeInfersDatabase(t *testing.T) {
	for _, rt := range []string{"db.m5.large", "db.r6g.xlarge"} {
		spec := map[string]interface{}{"resource_type": rt, "provider": "aws"}
		result := FillDomain(spec)
		if result["domain"] != "database" {
			t.Errorf("resource_type=%q: got %q want database", rt, result["domain"])
		}
	}
}

func TestFillDomainCacheResourceTypeInfersDatabase(t *testing.T) {
	for _, rt := range []string{"cache.r6g.large", "cache.t3.medium"} {
		spec := map[string]interface{}{"resource_type": rt, "provider": "aws"}
		result := FillDomain(spec)
		if result["domain"] != "database" {
			t.Errorf("resource_type=%q: got %q want database", rt, result["domain"])
		}
	}
}

func TestFillDomainComputeResourceTypePatterns(t *testing.T) {
	for _, rt := range []string{
		"m5.xlarge",
		"c6i.2xlarge",
		"n2-standard-4",
		"e2-medium",
		"Standard_D4s_v3",
		"Basic_A1",
		"Premium_P1",
	} {
		spec := map[string]interface{}{"resource_type": rt, "provider": "aws"}
		result := FillDomain(spec)
		if result["domain"] != "compute" {
			t.Errorf("resource_type=%q: got %q want compute", rt, result["domain"])
		}
	}
}

func TestFillDomainUnknownResourceTypeReturnsSpecUnchanged(t *testing.T) {
	spec := map[string]interface{}{"resource_type": "unknown", "provider": "aws"}
	result := FillDomain(spec)
	if _, ok := result["domain"]; ok {
		t.Errorf("expected no domain for unknown resource_type, got %q", result["domain"])
	}
}

// ---------------------------------------------------------------------------
// FillDomain — nothing to infer
// ---------------------------------------------------------------------------

func TestFillDomainEmptySpecReturnsUnchanged(t *testing.T) {
	spec := map[string]interface{}{}
	result := FillDomain(spec)
	if _, ok := result["domain"]; ok {
		t.Error("expected no domain for empty spec")
	}
}

func TestFillDomainUnknownServiceNoStorageOrResourceType(t *testing.T) {
	spec := map[string]interface{}{"service": "mysuperservice", "provider": "aws"}
	result := FillDomain(spec)
	if _, ok := result["domain"]; ok {
		t.Errorf("expected no domain for unknown service, got %q", result["domain"])
	}
}

// ---------------------------------------------------------------------------
// SpecErrorResponse — structure
// ---------------------------------------------------------------------------

func TestSpecErrorResponseHasRequiredKeys(t *testing.T) {
	result := SpecErrorResponse(errors.New("some error"), map[string]interface{}{})
	if result["error"] != "invalid_spec" {
		t.Errorf("error: got %q want invalid_spec", result["error"])
	}
	if _, ok := result["reason"]; !ok {
		t.Error("reason key missing")
	}
	if _, ok := result["hint"]; !ok {
		t.Error("hint key missing")
	}
}

func TestSpecErrorResponseReasonIsErrorMessage(t *testing.T) {
	exc := errors.New("boom boom")
	result := SpecErrorResponse(exc, map[string]interface{}{})
	if result["reason"] != "boom boom" {
		t.Errorf("reason: got %q want boom boom", result["reason"])
	}
}

// ---------------------------------------------------------------------------
// SpecErrorResponse — discriminator / domain hint
// ---------------------------------------------------------------------------

func TestSpecErrorResponseMissingDomainSetsFix(t *testing.T) {
	exc := errors.New("unable to extract tag using discriminator 'domain'")
	result := SpecErrorResponse(exc, map[string]interface{}{})
	fix, ok := result["fix"]
	if !ok {
		t.Fatal("fix key missing")
	}
	fixStr := fmt.Sprintf("%v", fix)
	if !strings.Contains(fixStr, "domain") || !strings.Contains(fixStr, "compute") {
		t.Errorf("fix must mention domain and compute: %q", fixStr)
	}
}

func TestSpecErrorResponseDiscriminatorErrorWithNoDomainInSpec(t *testing.T) {
	exc := errors.New("value error: discriminator field required")
	spec := map[string]interface{}{"service": "rds"} // no domain
	result := SpecErrorResponse(exc, spec)
	fix, ok := result["fix"]
	if !ok {
		t.Fatal("fix key missing")
	}
	fixStr := fmt.Sprintf("%v", fix)
	if !strings.Contains(fixStr, "domain") {
		t.Errorf("fix must mention domain: %q", fixStr)
	}
}

func TestSpecErrorResponseDiscriminatorWithDomainPresentNoFix(t *testing.T) {
	exc := errors.New("some discriminator error")
	spec := map[string]interface{}{"domain": "compute"}
	result := SpecErrorResponse(exc, spec)
	if fix, ok := result["fix"]; ok {
		fixStr := fmt.Sprintf("%v", fix)
		if strings.Contains(fixStr, "The 'domain' field") {
			t.Errorf("fix must not be domain hint when domain is present: %q", fixStr)
		}
	}
}

// ---------------------------------------------------------------------------
// SpecErrorResponse — bad term hint
// ---------------------------------------------------------------------------

func TestSpecErrorResponseBadTermListsValidTerms(t *testing.T) {
	exc := errors.New("value error: .term input should be one of on_demand, spot ...")
	result := SpecErrorResponse(exc, map[string]interface{}{"domain": "compute"})
	fix, ok := result["fix"]
	if !ok {
		t.Fatal("fix key missing")
	}
	fixStr := fmt.Sprintf("%v", fix)
	if !strings.Contains(fixStr, "on_demand") || !strings.Contains(fixStr, "spot") {
		t.Errorf("fix must list valid terms: %q", fixStr)
	}
}

// ---------------------------------------------------------------------------
// SpecErrorResponse — missing provider hint
// ---------------------------------------------------------------------------

func TestSpecErrorResponseMissingProviderSetsFix(t *testing.T) {
	exc := errors.New("provider field missing")
	result := SpecErrorResponse(exc, map[string]interface{}{"domain": "compute"})
	fix, ok := result["fix"]
	if !ok {
		t.Fatal("fix key missing")
	}
	fixStr := fmt.Sprintf("%v", fix)
	if !strings.Contains(fixStr, "provider") || !strings.Contains(fixStr, "aws") {
		t.Errorf("fix must mention provider and aws: %q", fixStr)
	}
}

// ---------------------------------------------------------------------------
// SpecErrorResponse — fallback (unrecognised error)
// ---------------------------------------------------------------------------

func TestSpecErrorResponseUnrecognisedErrorNoFix(t *testing.T) {
	exc := errors.New("something completely unexpected")
	result := SpecErrorResponse(exc, map[string]interface{}{"domain": "compute", "provider": "aws"})
	if result["error"] != "invalid_spec" {
		t.Errorf("error: got %q want invalid_spec", result["error"])
	}
	hint := fmt.Sprintf("%v", result["hint"])
	if !strings.Contains(hint, "describe_catalog") {
		t.Errorf("hint must mention describe_catalog: %q", hint)
	}
}
