package models

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ptr returns a pointer to v. Used for nullable optional fields in test fixtures.
func ptr[T any](v T) *T { return &v }

// --------------------------------------------------------------------------
// Round-trip tests — for each PricingSpec variant:
//   1. Build the struct
//   2. Marshal to JSON
//   3. UnmarshalPricingSpec from that JSON
//   4. Assert the round-tripped value equals the original
// --------------------------------------------------------------------------

func TestRoundTrip_ComputePricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ComputePricingSpec
	}{
		{
			name: "minimal with defaults",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainCompute,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				OS:            "Linux",
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "with instance type",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainCompute,
					Region:        "us-west-2",
					Term:          PricingTermReserved1Yr,
					SchemaVersion: "1",
				},
				ResourceType:  "m5.xlarge",
				OS:            "Linux",
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "fargate style with vcpu and memory",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainCompute,
					Service:       "fargate",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				OS:            "Linux",
				VCPU:          ptr(4.0),
				MemoryGB:      ptr(8.0),
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "windows OS",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainCompute,
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				ResourceType:  "n2-standard-4",
				OS:            "Windows",
				HoursPerMonth: 730.0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotC, ok := got.(*ComputePricingSpec)
			if !ok {
				t.Fatalf("expected *ComputePricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotC, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotC, tc.spec)
			}
		})
	}
}

func TestRoundTrip_StoragePricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec StoragePricingSpec
	}{
		{
			name: "defaults only",
			spec: StoragePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainStorage,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				StorageType: "gp3",
			},
		},
		{
			name: "with size and iops",
			spec: StoragePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainStorage,
					Region:        "eu-west-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				StorageType: "io1",
				SizeGB:      ptr(500.0),
				IOPS:        ptr(10000),
			},
		},
		{
			name: "gcp persistent disk",
			spec: StoragePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainStorage,
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				StorageType: "pd-ssd",
				SizeGB:      ptr(1000.0),
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotS, ok := got.(*StoragePricingSpec)
			if !ok {
				t.Fatalf("expected *StoragePricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotS, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotS, tc.spec)
			}
		})
	}
}

func TestRoundTrip_DatabasePricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec DatabasePricingSpec
	}{
		{
			name: "defaults",
			spec: DatabasePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainDatabase,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				ResourceType:  "",
				Engine:        "MySQL",
				Deployment:    "single-az",
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "rds postgres multi-az",
			spec: DatabasePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainDatabase,
					Service:       "rds",
					Region:        "us-east-1",
					Term:          PricingTermReserved1Yr,
					SchemaVersion: "1",
				},
				ResourceType:  "db.r5.xlarge",
				Engine:        "PostgreSQL",
				Deployment:    "multi-az",
				StorageGB:     ptr(100.0),
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "elasticache with capacity",
			spec: DatabasePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainDatabase,
					Service:       "elasticache",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				ResourceType:  "cache.r6g.large",
				Engine:        "Redis",
				Deployment:    "single-az",
				CapacityGB:    ptr(6.38),
				HoursPerMonth: 730.0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotD, ok := got.(*DatabasePricingSpec)
			if !ok {
				t.Fatalf("expected *DatabasePricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotD, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotD, tc.spec)
			}
		})
	}
}

func TestRoundTrip_ContainerPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ContainerPricingSpec
	}{
		{
			name: "defaults",
			spec: ContainerPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainContainer,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				Mode:          "standard",
				NodeCount:     3,
				HoursPerMonth: 730.0,
			},
		},
		{
			name: "gke autopilot",
			spec: ContainerPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainContainer,
					Service:       "gke",
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				Mode:          "autopilot",
				NodeCount:     3,
				VCPU:          ptr(2.0),
				MemoryGB:      ptr(4.0),
				HoursPerMonth: 730.0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotC, ok := got.(*ContainerPricingSpec)
			if !ok {
				t.Fatalf("expected *ContainerPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotC, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotC, tc.spec)
			}
		})
	}
}

func TestRoundTrip_AiPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec AiPricingSpec
	}{
		{
			name: "defaults",
			spec: AiPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainAI,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				Task: "inference",
				Mode: "on_demand",
			},
		},
		{
			name: "bedrock claude",
			spec: AiPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainAI,
					Service:       "bedrock",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				Model:        "claude-3-5-sonnet",
				Task:         "inference",
				InputTokens:  ptr(1000000),
				OutputTokens: ptr(500000),
				Mode:         "on_demand",
			},
		},
		{
			name: "vertex training",
			spec: AiPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainAI,
					Service:       "vertex",
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				MachineType:   "n1-standard-8",
				Task:          "training",
				TrainingHours: ptr(48.0),
				Mode:          "on_demand",
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotA, ok := got.(*AiPricingSpec)
			if !ok {
				t.Fatalf("expected *AiPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotA, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotA, tc.spec)
			}
		})
	}
}

func TestRoundTrip_ServerlessPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ServerlessPricingSpec
	}{
		{
			name: "defaults",
			spec: ServerlessPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainServerless,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
			},
		},
		{
			name: "lambda with gb-seconds",
			spec: ServerlessPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainServerless,
					Service:       "lambda",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				GBSeconds:        ptr(1000000.0),
				RequestsMillions: ptr(1.0),
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotS, ok := got.(*ServerlessPricingSpec)
			if !ok {
				t.Fatalf("expected *ServerlessPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotS, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotS, tc.spec)
			}
		})
	}
}

func TestRoundTrip_AnalyticsPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec AnalyticsPricingSpec
	}{
		{
			name: "defaults",
			spec: AnalyticsPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainAnalytics,
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
			},
		},
		{
			name: "bigquery with query and storage",
			spec: AnalyticsPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderGCP,
					Domain:        PricingDomainAnalytics,
					Service:       "bigquery",
					Region:        "us-central1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				QueryTB:           ptr(10.0),
				ActiveStorageGB:   ptr(1000.0),
				LongtermStorageGB: ptr(5000.0),
				StreamingGB:       ptr(50.0),
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotA, ok := got.(*AnalyticsPricingSpec)
			if !ok {
				t.Fatalf("expected *AnalyticsPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotA, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotA, tc.spec)
			}
		})
	}
}

func TestRoundTrip_NetworkPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec NetworkPricingSpec
	}{
		{
			name: "defaults",
			spec: NetworkPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainNetwork,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				RuleCount:       1,
				GatewayCount:    1,
				PolicyCount:     1,
				HoursPerMonth:   730.0,
				DestinationType: "internet",
				NetworkTier:     "premium",
			},
		},
		{
			name: "load balancer",
			spec: NetworkPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainNetwork,
					Service:       "lb",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				LBType:          "application",
				RuleCount:       10,
				DataGB:          1000.0,
				GatewayCount:    1,
				PolicyCount:     1,
				HoursPerMonth:   730.0,
				DestinationType: "internet",
				NetworkTier:     "premium",
			},
		},
		{
			name: "egress service branch",
			spec: NetworkPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainNetwork,
					Service:       "egress",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				RuleCount:         1,
				GatewayCount:      1,
				PolicyCount:       1,
				HoursPerMonth:     730.0,
				SourceRegion:      "us-east-1",
				DestinationType:   "internet",
				DestinationRegion: "",
				DataGBPerMonth:    500.0,
				NetworkTier:       "premium",
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotN, ok := got.(*NetworkPricingSpec)
			if !ok {
				t.Fatalf("expected *NetworkPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotN, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotN, tc.spec)
			}
		})
	}
}

func TestRoundTrip_ObservabilityPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ObservabilityPricingSpec
	}{
		{
			name: "defaults",
			spec: ObservabilityPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainObservability,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
			},
		},
		{
			name: "cloudwatch with values",
			spec: ObservabilityPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainObservability,
					Service:       "cloudwatch",
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				IngestionMiB: 100.0,
				MetricsCount: 500,
				LogGB:        50.0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotO, ok := got.(*ObservabilityPricingSpec)
			if !ok {
				t.Fatalf("expected *ObservabilityPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotO, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotO, tc.spec)
			}
		})
	}
}

func TestRoundTrip_EgressPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec EgressPricingSpec
	}{
		{
			name: "defaults",
			spec: EgressPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainInterRegionEgress,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				DataGB: 1.0,
			},
		},
		{
			name: "cross-region with dest",
			spec: EgressPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider:      CloudProviderAWS,
					Domain:        PricingDomainInterRegionEgress,
					Region:        "us-east-1",
					Term:          PricingTermOnDemand,
					SchemaVersion: "1",
				},
				SourceRegion: "us-east-1",
				DestRegion:   "eu-west-1",
				DataGB:       1000.0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(&tc.spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalPricingSpec(data)
			if err != nil {
				t.Fatalf("UnmarshalPricingSpec: %v", err)
			}
			gotE, ok := got.(*EgressPricingSpec)
			if !ok {
				t.Fatalf("expected *EgressPricingSpec, got %T", got)
			}
			if !reflect.DeepEqual(*gotE, tc.spec) {
				t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", *gotE, tc.spec)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Default-restoration tests — verify fields absent from JSON get defaults.
// --------------------------------------------------------------------------

func TestUnmarshal_ComputeDefaults(t *testing.T) {
	// Minimal JSON: only required discriminator + provider + region.
	data := []byte(`{"domain":"compute","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	c, ok := got.(*ComputePricingSpec)
	if !ok {
		t.Fatalf("expected *ComputePricingSpec, got %T", got)
	}
	if c.OS != "Linux" {
		t.Errorf("OS default: got %q, want %q", c.OS, "Linux")
	}
	if c.HoursPerMonth != 730.0 {
		t.Errorf("HoursPerMonth default: got %v, want 730.0", c.HoursPerMonth)
	}
	if c.Term != PricingTermOnDemand {
		t.Errorf("Term default: got %q, want %q", c.Term, PricingTermOnDemand)
	}
	if c.SchemaVersion != "1" {
		t.Errorf("SchemaVersion default: got %q, want %q", c.SchemaVersion, "1")
	}
}

func TestUnmarshal_StorageDefaults(t *testing.T) {
	data := []byte(`{"domain":"storage","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	s, ok := got.(*StoragePricingSpec)
	if !ok {
		t.Fatalf("expected *StoragePricingSpec, got %T", got)
	}
	if s.StorageType != "gp3" {
		t.Errorf("StorageType default: got %q, want %q", s.StorageType, "gp3")
	}
	if s.Term != PricingTermOnDemand {
		t.Errorf("Term default: got %q, want %q", s.Term, PricingTermOnDemand)
	}
}

func TestUnmarshal_DatabaseDefaults(t *testing.T) {
	data := []byte(`{"domain":"database","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	d, ok := got.(*DatabasePricingSpec)
	if !ok {
		t.Fatalf("expected *DatabasePricingSpec, got %T", got)
	}
	if d.Engine != "MySQL" {
		t.Errorf("Engine default: got %q, want %q", d.Engine, "MySQL")
	}
	if d.Deployment != "single-az" {
		t.Errorf("Deployment default: got %q, want %q", d.Deployment, "single-az")
	}
	if d.HoursPerMonth != 730.0 {
		t.Errorf("HoursPerMonth default: got %v, want 730.0", d.HoursPerMonth)
	}
}

func TestUnmarshal_ContainerDefaults(t *testing.T) {
	data := []byte(`{"domain":"container","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	c, ok := got.(*ContainerPricingSpec)
	if !ok {
		t.Fatalf("expected *ContainerPricingSpec, got %T", got)
	}
	if c.Mode != "standard" {
		t.Errorf("Mode default: got %q, want %q", c.Mode, "standard")
	}
	if c.NodeCount != 3 {
		t.Errorf("NodeCount default: got %d, want 3", c.NodeCount)
	}
}

func TestUnmarshal_AiDefaults(t *testing.T) {
	data := []byte(`{"domain":"ai","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	a, ok := got.(*AiPricingSpec)
	if !ok {
		t.Fatalf("expected *AiPricingSpec, got %T", got)
	}
	if a.Task != "inference" {
		t.Errorf("Task default: got %q, want %q", a.Task, "inference")
	}
	if a.Mode != "on_demand" {
		t.Errorf("Mode default: got %q, want %q", a.Mode, "on_demand")
	}
}

func TestUnmarshal_NetworkDefaults(t *testing.T) {
	data := []byte(`{"domain":"network","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	n, ok := got.(*NetworkPricingSpec)
	if !ok {
		t.Fatalf("expected *NetworkPricingSpec, got %T", got)
	}
	if n.RuleCount != 1 {
		t.Errorf("RuleCount default: got %d, want 1", n.RuleCount)
	}
	if n.GatewayCount != 1 {
		t.Errorf("GatewayCount default: got %d, want 1", n.GatewayCount)
	}
	if n.PolicyCount != 1 {
		t.Errorf("PolicyCount default: got %d, want 1", n.PolicyCount)
	}
	if n.HoursPerMonth != 730.0 {
		t.Errorf("HoursPerMonth default: got %v, want 730.0", n.HoursPerMonth)
	}
	if n.DestinationType != "internet" {
		t.Errorf("DestinationType default: got %q, want %q", n.DestinationType, "internet")
	}
	if n.NetworkTier != "premium" {
		t.Errorf("NetworkTier default: got %q, want %q", n.NetworkTier, "premium")
	}
}

func TestUnmarshal_EgressDefaults(t *testing.T) {
	data := []byte(`{"domain":"inter_region_egress","provider":"aws","region":"us-east-1"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	e, ok := got.(*EgressPricingSpec)
	if !ok {
		t.Fatalf("expected *EgressPricingSpec, got %T", got)
	}
	if e.DataGB != 1.0 {
		t.Errorf("DataGB default: got %v, want 1.0", e.DataGB)
	}
}

// --------------------------------------------------------------------------
// instance_type alias test — ComputePricingSpec model_validator parity.
// --------------------------------------------------------------------------

func TestUnmarshal_ComputeInstanceTypeAlias(t *testing.T) {
	data := []byte(`{"domain":"compute","provider":"aws","region":"us-east-1","instance_type":"m5.xlarge"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	c, ok := got.(*ComputePricingSpec)
	if !ok {
		t.Fatalf("expected *ComputePricingSpec, got %T", got)
	}
	if c.ResourceType != "m5.xlarge" {
		t.Errorf("ResourceType: got %q, want %q", c.ResourceType, "m5.xlarge")
	}
}

func TestUnmarshal_ComputeResourceTypeWins(t *testing.T) {
	// When both are present, resource_type wins over instance_type.
	data := []byte(`{"domain":"compute","provider":"aws","region":"us-east-1","resource_type":"t3.medium","instance_type":"m5.xlarge"}`)
	got, err := UnmarshalPricingSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPricingSpec: %v", err)
	}
	c, ok := got.(*ComputePricingSpec)
	if !ok {
		t.Fatalf("expected *ComputePricingSpec, got %T", got)
	}
	if c.ResourceType != "t3.medium" {
		t.Errorf("ResourceType: got %q, want %q", c.ResourceType, "t3.medium")
	}
}

// --------------------------------------------------------------------------
// CacheKey tests — one per variant, including NetworkPricingSpec egress branch.
// --------------------------------------------------------------------------

func TestCacheKey_ComputePricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ComputePricingSpec
		want string
	}{
		{
			name: "minimal",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainCompute,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				OS:            "Linux",
				HoursPerMonth: 730.0,
			},
			// format: base:resource_type:os:vcpu:memory_gb
			want: "aws:compute::us-east-1:on_demand::Linux::",
		},
		{
			name: "with instance type",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainCompute,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				ResourceType:  "m5.xlarge",
				OS:            "Linux",
				HoursPerMonth: 730.0,
			},
			want: "aws:compute::us-east-1:on_demand:m5.xlarge:Linux::",
		},
		{
			name: "fargate with vcpu and memory",
			spec: ComputePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainCompute,
					Service:  "fargate",
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				OS:            "Linux",
				VCPU:          ptr(4.0),
				MemoryGB:      ptr(8.0),
				HoursPerMonth: 730.0,
			},
			want: "aws:compute:fargate:us-east-1:on_demand::Linux:4:8",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCacheKey_StoragePricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec StoragePricingSpec
		want string
	}{
		{
			name: "no size or iops",
			spec: StoragePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainStorage,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				StorageType: "gp3",
			},
			want: "aws:storage::us-east-1:on_demand:gp3::",
		},
		{
			name: "with size and iops",
			spec: StoragePricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainStorage,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				StorageType: "io1",
				SizeGB:      ptr(100.0),
				IOPS:        ptr(3000),
			},
			want: "aws:storage::us-east-1:on_demand:io1:100:3000",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCacheKey_DatabasePricingSpec(t *testing.T) {
	spec := DatabasePricingSpec{
		BasePricingSpec: BasePricingSpec{
			Provider: CloudProviderAWS,
			Domain:   PricingDomainDatabase,
			Service:  "rds",
			Region:   "us-east-1",
			Term:     PricingTermOnDemand,
		},
		ResourceType: "db.r5.xlarge",
		Engine:       "PostgreSQL",
		Deployment:   "multi-az",
	}
	got := spec.CacheKey()
	want := "aws:database:rds:us-east-1:on_demand:db.r5.xlarge:PostgreSQL:multi-az"
	if got != want {
		t.Errorf("CacheKey() = %q, want %q", got, want)
	}
}

func TestCacheKey_ContainerPricingSpec(t *testing.T) {
	spec := ContainerPricingSpec{
		BasePricingSpec: BasePricingSpec{
			Provider: CloudProviderGCP,
			Domain:   PricingDomainContainer,
			Service:  "gke",
			Region:   "us-central1",
			Term:     PricingTermOnDemand,
		},
		Mode:      "autopilot",
		NodeType:  "",
		NodeCount: 3,
	}
	got := spec.CacheKey()
	want := "gcp:container:gke:us-central1:on_demand:autopilot::3"
	if got != want {
		t.Errorf("CacheKey() = %q, want %q", got, want)
	}
}

func TestCacheKey_AiPricingSpec(t *testing.T) {
	spec := AiPricingSpec{
		BasePricingSpec: BasePricingSpec{
			Provider: CloudProviderAWS,
			Domain:   PricingDomainAI,
			Service:  "bedrock",
			Region:   "us-east-1",
			Term:     PricingTermOnDemand,
		},
		Model: "claude-3-5-sonnet",
		Task:  "inference",
		Mode:  "on_demand",
	}
	got := spec.CacheKey()
	want := "aws:ai:bedrock:us-east-1:on_demand:claude-3-5-sonnet::inference:on_demand"
	if got != want {
		t.Errorf("CacheKey() = %q, want %q", got, want)
	}
}

func TestCacheKey_ServerlessPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ServerlessPricingSpec
		want string
	}{
		{
			name: "no sizing",
			spec: ServerlessPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainServerless,
					Service:  "lambda",
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
			},
			want: "aws:serverless:lambda:us-east-1:on_demand::",
		},
		{
			name: "with gb-seconds and requests",
			spec: ServerlessPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainServerless,
					Service:  "lambda",
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				GBSeconds:        ptr(1000000.0),
				RequestsMillions: ptr(1.0),
			},
			want: "aws:serverless:lambda:us-east-1:on_demand:1e+06:1",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCacheKey_AnalyticsPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec AnalyticsPricingSpec
		want string
	}{
		{
			name: "no values",
			spec: AnalyticsPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderGCP,
					Domain:   PricingDomainAnalytics,
					Service:  "bigquery",
					Region:   "us-central1",
					Term:     PricingTermOnDemand,
				},
			},
			want: "gcp:analytics:bigquery:us-central1:on_demand::",
		},
		{
			name: "with query and storage",
			spec: AnalyticsPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderGCP,
					Domain:   PricingDomainAnalytics,
					Service:  "bigquery",
					Region:   "us-central1",
					Term:     PricingTermOnDemand,
				},
				QueryTB:         ptr(10.0),
				ActiveStorageGB: ptr(1000.0),
			},
			want: "gcp:analytics:bigquery:us-central1:on_demand:10:1000",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCacheKey_NetworkPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec NetworkPricingSpec
		want string
	}{
		{
			name: "load balancer (non-egress)",
			spec: NetworkPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainNetwork,
					Service:  "lb",
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				LBType:       "application",
				RuleCount:    10,
				GatewayCount: 2,
			},
			want: "aws:network:lb:us-east-1:on_demand:application:10:2",
		},
		{
			name: "egress branch",
			spec: NetworkPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainNetwork,
					Service:  "egress",
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				SourceRegion:    "us-east-1",
				DestinationType: "internet",
				DataGBPerMonth:  500.0,
				NetworkTier:     "premium",
			},
			want: "aws:network:egress:us-east-1:on_demand:us-east-1:internet::500:premium",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCacheKey_ObservabilityPricingSpec(t *testing.T) {
	spec := ObservabilityPricingSpec{
		BasePricingSpec: BasePricingSpec{
			Provider: CloudProviderAWS,
			Domain:   PricingDomainObservability,
			Service:  "cloudwatch",
			Region:   "us-east-1",
			Term:     PricingTermOnDemand,
		},
		IngestionMiB: 100.0,
		MetricsCount: 500,
		LogGB:        50.0,
	}
	got := spec.CacheKey()
	want := "aws:observability:cloudwatch:us-east-1:on_demand:100:500:50"
	if got != want {
		t.Errorf("CacheKey() = %q, want %q", got, want)
	}
}

func TestCacheKey_EgressPricingSpec(t *testing.T) {
	tests := []struct {
		name string
		spec EgressPricingSpec
		want string
	}{
		{
			name: "internet egress",
			spec: EgressPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainInterRegionEgress,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				SourceRegion: "us-east-1",
				DataGB:       1.0,
			},
			want: "aws:inter_region_egress::us-east-1:on_demand:us-east-1:",
		},
		{
			name: "region-to-region",
			spec: EgressPricingSpec{
				BasePricingSpec: BasePricingSpec{
					Provider: CloudProviderAWS,
					Domain:   PricingDomainInterRegionEgress,
					Region:   "us-east-1",
					Term:     PricingTermOnDemand,
				},
				SourceRegion: "us-east-1",
				DestRegion:   "eu-west-1",
				DataGB:       1000.0,
			},
			want: "aws:inter_region_egress::us-east-1:on_demand:us-east-1:eu-west-1",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.CacheKey()
			if got != tc.want {
				t.Errorf("CacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// UnmarshalPricingSpec error cases.
// --------------------------------------------------------------------------

func TestUnmarshalPricingSpec_UnknownDomain(t *testing.T) {
	data := []byte(`{"domain":"unknown_domain","provider":"aws","region":"us-east-1"}`)
	_, err := UnmarshalPricingSpec(data)
	if err == nil {
		t.Error("expected error for unknown domain, got nil")
	}
}

func TestUnmarshalPricingSpec_InvalidJSON(t *testing.T) {
	_, err := UnmarshalPricingSpec([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestUnmarshalPricingSpec_MissingDomain(t *testing.T) {
	data := []byte(`{"provider":"aws","region":"us-east-1"}`)
	_, err := UnmarshalPricingSpec(data)
	if err == nil {
		t.Error("expected error for missing domain, got nil")
	}
}

// --------------------------------------------------------------------------
// NormalizedPrice computed properties.
// --------------------------------------------------------------------------

func TestNormalizedPrice_MonthlyCost(t *testing.T) {
	tests := []struct {
		name  string
		price float64
		unit  PriceUnit
		want  float64
	}{
		{"per_hour", 0.192, PriceUnitPerHour, 0.192 * 730},
		{"per_month", 100.0, PriceUnitPerMonth, 100.0},
		{"per_gb", 0.023, PriceUnitPerGB, 0.023},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			n := &NormalizedPrice{PricePerUnit: tc.price, Unit: tc.unit}
			got := n.MonthlyCost()
			if got != tc.want {
				t.Errorf("MonthlyCost() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizedPrice_HourlyCost(t *testing.T) {
	tests := []struct {
		name  string
		price float64
		unit  PriceUnit
		want  float64
	}{
		{"per_hour", 0.192, PriceUnitPerHour, 0.192},
		{"per_month", 730.0, PriceUnitPerMonth, 1.0},
		{"per_gb", 0.023, PriceUnitPerGB, 0.023},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			n := &NormalizedPrice{PricePerUnit: tc.price, Unit: tc.unit}
			got := n.HourlyCost()
			if got != tc.want {
				t.Errorf("HourlyCost() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Interface compliance — all concrete types implement PricingSpec via
// GetProvider, GetDomain, GetService, GetRegion, GetTerm, CacheKey.
// --------------------------------------------------------------------------

func TestPricingSpecInterface_GetterMethods(t *testing.T) {
	base := BasePricingSpec{
		Provider:      CloudProviderAWS,
		Domain:        PricingDomainCompute,
		Service:       "ec2",
		Region:        "us-east-1",
		Term:          PricingTermOnDemand,
		SchemaVersion: "1",
	}

	specs := []PricingSpec{
		&ComputePricingSpec{BasePricingSpec: base, OS: "Linux", HoursPerMonth: 730},
		&StoragePricingSpec{BasePricingSpec: base, StorageType: "gp3"},
		&DatabasePricingSpec{BasePricingSpec: base, Engine: "MySQL", Deployment: "single-az"},
		&ContainerPricingSpec{BasePricingSpec: base, Mode: "standard", NodeCount: 3},
		&AiPricingSpec{BasePricingSpec: base, Task: "inference", Mode: "on_demand"},
		&ServerlessPricingSpec{BasePricingSpec: base},
		&AnalyticsPricingSpec{BasePricingSpec: base},
		&NetworkPricingSpec{BasePricingSpec: base, RuleCount: 1, GatewayCount: 1, PolicyCount: 1, HoursPerMonth: 730, DestinationType: "internet", NetworkTier: "premium"},
		&ObservabilityPricingSpec{BasePricingSpec: base},
		&EgressPricingSpec{BasePricingSpec: base, DataGB: 1.0},
	}

	for _, spec := range specs {
		if got := spec.GetProvider(); got != CloudProviderAWS {
			t.Errorf("%T.GetProvider() = %q, want %q", spec, got, CloudProviderAWS)
		}
		if got := spec.GetRegion(); got != "us-east-1" {
			t.Errorf("%T.GetRegion() = %q, want %q", spec, got, "us-east-1")
		}
		if got := spec.GetTerm(); got != PricingTermOnDemand {
			t.Errorf("%T.GetTerm() = %q, want %q", spec, got, PricingTermOnDemand)
		}
		// CacheKey must be non-empty and stable.
		k1 := spec.CacheKey()
		k2 := spec.CacheKey()
		if k1 == "" {
			t.Errorf("%T.CacheKey() is empty", spec)
		}
		if k1 != k2 {
			t.Errorf("%T.CacheKey() not stable: %q vs %q", spec, k1, k2)
		}
	}
}

// --------------------------------------------------------------------------
// Enum string values — must match Python exactly.
// --------------------------------------------------------------------------

func TestEnumValues_CloudProvider(t *testing.T) {
	tests := []struct {
		val  CloudProvider
		want string
	}{
		{CloudProviderAWS, "aws"},
		{CloudProviderGCP, "gcp"},
		{CloudProviderAzure, "azure"},
	}
	for _, tc := range tests {
		if string(tc.val) != tc.want {
			t.Errorf("CloudProvider %v = %q, want %q", tc.val, string(tc.val), tc.want)
		}
	}
}

func TestEnumValues_PricingTerm(t *testing.T) {
	tests := []struct {
		val  PricingTerm
		want string
	}{
		{PricingTermOnDemand, "on_demand"},
		{PricingTermReserved1Yr, "reserved_1yr"},
		{PricingTermReserved3Yr, "reserved_3yr"},
		{PricingTermReserved1YrPartial, "reserved_1yr_partial"},
		{PricingTermReserved1YrAll, "reserved_1yr_all"},
		{PricingTermReserved3YrPartial, "reserved_3yr_partial"},
		{PricingTermReserved3YrAll, "reserved_3yr_all"},
		{PricingTermSpot, "spot"},
		{PricingTermSavingsPlan, "savings_plan"},
		{PricingTermComputeSP, "compute_savings_plan"},
		{PricingTermEC2InstanceSP, "ec2_instance_savings_plan"},
		{PricingTermSageMakerSP, "sagemaker_savings_plan"},
		{PricingTermCUD1Yr, "cud_1yr"},
		{PricingTermCUD3Yr, "cud_3yr"},
		{PricingTermFlexCUD, "flex_cud"},
		{PricingTermSUD, "sud"},
		{PricingTermPTU, "provisioned_throughput_units"},
	}
	for _, tc := range tests {
		if string(tc.val) != tc.want {
			t.Errorf("PricingTerm %v = %q, want %q", tc.val, string(tc.val), tc.want)
		}
	}
}

func TestEnumValues_PricingDomain(t *testing.T) {
	tests := []struct {
		val  PricingDomain
		want string
	}{
		{PricingDomainCompute, "compute"},
		{PricingDomainStorage, "storage"},
		{PricingDomainDatabase, "database"},
		{PricingDomainContainer, "container"},
		{PricingDomainAI, "ai"},
		{PricingDomainServerless, "serverless"},
		{PricingDomainAnalytics, "analytics"},
		{PricingDomainNetwork, "network"},
		{PricingDomainObservability, "observability"},
		{PricingDomainInterRegionEgress, "inter_region_egress"},
	}
	for _, tc := range tests {
		if string(tc.val) != tc.want {
			t.Errorf("PricingDomain %v = %q, want %q", tc.val, string(tc.val), tc.want)
		}
	}
}

func TestEnumValues_PriceUnit(t *testing.T) {
	tests := []struct {
		val  PriceUnit
		want string
	}{
		{PriceUnitPerHour, "per_hour"},
		{PriceUnitPerMonth, "per_month"},
		{PriceUnitPerGBMonth, "per_gb_month"},
		{PriceUnitPerGB, "per_gb"},
		{PriceUnitPerIOPSMonth, "per_iops_month"},
		{PriceUnitPerMBPSMonth, "per_mbps_month"},
		{PriceUnitPerRequest, "per_request"},
		{PriceUnitPerGBSecond, "per_gb_second"},
		{PriceUnitPerQuery, "per_query"},
		{PriceUnitPerUnit, "per_unit"},
	}
	for _, tc := range tests {
		if string(tc.val) != tc.want {
			t.Errorf("PriceUnit %v = %q, want %q", tc.val, string(tc.val), tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// TestPricingSpec_RequiredFields — UnmarshalPricingSpec requires the domain
// field; without it the call returns an error (mirrors Python Pydantic
// validation that domain and provider are required on BasePricingSpec).
// --------------------------------------------------------------------------

func TestPricingSpec_RequiredFields(t *testing.T) {
	// Missing domain: UnmarshalPricingSpec must return an error because it
	// cannot determine which concrete spec variant to decode into.
	t.Run("missing domain returns error", func(t *testing.T) {
		data := []byte(`{"provider":"aws","region":"us-east-1","term":"on_demand"}`)
		_, err := UnmarshalPricingSpec(data)
		if err == nil {
			t.Error("expected error when domain is absent, got nil")
		}
	})

	// Empty domain string: also invalid — no variant matches "".
	t.Run("empty domain string returns error", func(t *testing.T) {
		data := []byte(`{"domain":"","provider":"aws","region":"us-east-1"}`)
		_, err := UnmarshalPricingSpec(data)
		if err == nil {
			t.Error("expected error when domain is empty string, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// TestNormalizedPrice_ZeroValueIsValid — zero price_per_unit is valid (free tier).
// --------------------------------------------------------------------------

func TestNormalizedPrice_ZeroValueIsValid(t *testing.T) {
	n := &NormalizedPrice{
		Provider:      CloudProviderAWS,
		Service:       "compute",
		SKUID:         "FREE123",
		ProductFamily: "Compute Instance",
		Description:   "free tier t2.micro",
		Region:        "us-east-1",
		PricingTerm:   PricingTermOnDemand,
		PricePerUnit:  0.0,
		Unit:          PriceUnitPerHour,
	}

	// Zero price_per_unit is a valid struct — no panic, no error.
	if n.PricePerUnit != 0.0 {
		t.Errorf("expected PricePerUnit=0.0, got %v", n.PricePerUnit)
	}

	// MonthlyCost of a free-tier item should be 0.
	mc := n.MonthlyCost()
	if mc != 0.0 {
		t.Errorf("MonthlyCost() for zero price = %v, want 0.0", mc)
	}

	// HourlyCost of a free-tier item should also be 0.
	hc := n.HourlyCost()
	if hc != 0.0 {
		t.Errorf("HourlyCost() for zero price = %v, want 0.0", hc)
	}
}
