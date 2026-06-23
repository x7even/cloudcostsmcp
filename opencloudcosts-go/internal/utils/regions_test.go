package utils

import (
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// AWSRegionToDisplay
// ---------------------------------------------------------------------------

func TestAWSRegionToDisplayKnown(t *testing.T) {
	display, err := AWSRegionToDisplay("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if display != "US East (N. Virginia)" {
		t.Errorf("got %q want %q", display, "US East (N. Virginia)")
	}
}

func TestAWSRegionToDisplayUnknown(t *testing.T) {
	_, err := AWSRegionToDisplay("xx-invalid-99")
	if err == nil {
		t.Error("expected error for unknown region code")
	}
}

// ---------------------------------------------------------------------------
// AWSDisplayToRegion
// ---------------------------------------------------------------------------

func TestAWSDisplayToRegionKnown(t *testing.T) {
	code, err := AWSDisplayToRegion("US East (N. Virginia)")
	if err != nil {
		t.Fatal(err)
	}
	if code != "us-east-1" {
		t.Errorf("got %q want us-east-1", code)
	}
}

func TestAWSDisplayToRegionUnknown(t *testing.T) {
	_, err := AWSDisplayToRegion("Nowhere Land")
	if err == nil {
		t.Error("expected error for unknown display name")
	}
}

// ---------------------------------------------------------------------------
// NormalizeRegion — AWS
// ---------------------------------------------------------------------------

func TestNormalizeRegionAWSCodePassthrough(t *testing.T) {
	code, err := NormalizeRegion("aws", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if code != "us-west-2" {
		t.Errorf("got %q want us-west-2", code)
	}
}

func TestNormalizeRegionAWSDisplayToCode(t *testing.T) {
	code, err := NormalizeRegion("aws", "US West (Oregon)")
	if err != nil {
		t.Fatal(err)
	}
	if code != "us-west-2" {
		t.Errorf("got %q want us-west-2", code)
	}
}

func TestNormalizeRegionAWSUnknownErrors(t *testing.T) {
	_, err := NormalizeRegion("aws", "totally-unknown")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// NormalizeRegion — GCP
// ---------------------------------------------------------------------------

func TestNormalizeRegionGCPCodePassthrough(t *testing.T) {
	code, err := NormalizeRegion("gcp", "us-central1")
	if err != nil {
		t.Fatal(err)
	}
	if code != "us-central1" {
		t.Errorf("got %q want us-central1", code)
	}
}

func TestNormalizeRegionGCPDisplayToCode(t *testing.T) {
	code, err := NormalizeRegion("gcp", "US Central (Iowa)")
	if err != nil {
		t.Fatal(err)
	}
	if code != "us-central1" {
		t.Errorf("got %q want us-central1", code)
	}
}

func TestNormalizeRegionGCPUnknownErrors(t *testing.T) {
	_, err := NormalizeRegion("gcp", "xx-invalid")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// NormalizeRegion — Azure
// ---------------------------------------------------------------------------

func TestNormalizeRegionAzureCodePassthrough(t *testing.T) {
	code, err := NormalizeRegion("azure", "eastus")
	if err != nil {
		t.Fatal(err)
	}
	if code != "eastus" {
		t.Errorf("got %q want eastus", code)
	}
}

func TestNormalizeRegionAzureDisplayToCode(t *testing.T) {
	code, err := NormalizeRegion("azure", "East US")
	if err != nil {
		t.Fatal(err)
	}
	if code != "eastus" {
		t.Errorf("got %q want eastus", code)
	}
}

func TestNormalizeRegionAzureUnknownErrors(t *testing.T) {
	_, err := NormalizeRegion("azure", "nowhere")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// NormalizeRegion — unknown provider
// ---------------------------------------------------------------------------

func TestNormalizeRegionUnknownProviderPassthrough(t *testing.T) {
	code, err := NormalizeRegion("unknownprovider", "some-region")
	if err != nil {
		t.Fatal(err)
	}
	if code != "some-region" {
		t.Errorf("got %q want some-region", code)
	}
}

// ---------------------------------------------------------------------------
// ListAWSRegions
// ---------------------------------------------------------------------------

func TestListAWSRegionsSorted(t *testing.T) {
	regions := ListAWSRegions()
	if len(regions) == 0 {
		t.Fatal("expected non-empty list")
	}
	if !sort.StringsAreSorted(regions) {
		t.Error("ListAWSRegions must return sorted list")
	}
	// spot-check
	found := false
	for _, r := range regions {
		if r == "us-east-1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("us-east-1 not found in ListAWSRegions")
	}
}

// ---------------------------------------------------------------------------
// ListGCPRegions
// ---------------------------------------------------------------------------

func TestListGCPRegionsSorted(t *testing.T) {
	regions := ListGCPRegions()
	if len(regions) == 0 {
		t.Fatal("expected non-empty list")
	}
	if !sort.StringsAreSorted(regions) {
		t.Error("ListGCPRegions must return sorted list")
	}
	found := false
	for _, r := range regions {
		if r == "us-central1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("us-central1 not found in ListGCPRegions")
	}
}

// ---------------------------------------------------------------------------
// ListAzureRegions
// ---------------------------------------------------------------------------

func TestListAzureRegionsSorted(t *testing.T) {
	regions := ListAzureRegions()
	if len(regions) == 0 {
		t.Fatal("expected non-empty list")
	}
	if !sort.StringsAreSorted(regions) {
		t.Error("ListAzureRegions must return sorted list")
	}
	found := false
	for _, r := range regions {
		if r == "eastus" {
			found = true
			break
		}
	}
	if !found {
		t.Error("eastus not found in ListAzureRegions")
	}
}

// ---------------------------------------------------------------------------
// RegionDisplayName
// ---------------------------------------------------------------------------

func TestRegionDisplayNameAWS(t *testing.T) {
	name := RegionDisplayName("aws", "us-east-1")
	if name != "US East (N. Virginia)" {
		t.Errorf("got %q want %q", name, "US East (N. Virginia)")
	}
}

func TestRegionDisplayNameGCP(t *testing.T) {
	name := RegionDisplayName("gcp", "europe-west1")
	if name != "Europe (Belgium)" {
		t.Errorf("got %q want %q", name, "Europe (Belgium)")
	}
}

func TestRegionDisplayNameAzure(t *testing.T) {
	name := RegionDisplayName("azure", "eastus")
	if name != "East US" {
		t.Errorf("got %q want %q", name, "East US")
	}
}

func TestRegionDisplayNameUnknownReturnsCode(t *testing.T) {
	name := RegionDisplayName("aws", "xx-unknown-9")
	if name != "xx-unknown-9" {
		t.Errorf("expected passthrough, got %q", name)
	}
}

// ---------------------------------------------------------------------------
// Bidirectional round-trips
// ---------------------------------------------------------------------------

func TestAWSRoundTrip(t *testing.T) {
	for code := range AWSRegionDisplay {
		display, err := AWSRegionToDisplay(code)
		if err != nil {
			t.Errorf("AWSRegionToDisplay(%q): %v", code, err)
			continue
		}
		back, err := AWSDisplayToRegion(display)
		if err != nil {
			t.Errorf("AWSDisplayToRegion(%q): %v", display, err)
			continue
		}
		if back != code {
			t.Errorf("round-trip failed: %q → %q → %q", code, display, back)
		}
	}
}
