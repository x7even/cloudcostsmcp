// Package models defines the provider-agnostic data types shared across all
// cloud pricing providers. The concrete PricingSpec variants and their
// discriminated-union UnmarshalJSON are implemented by Agent 1A (Phase 1).
// This file provides the minimal type surface that the Provider interface
// and Phase 2 agents depend on.
package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CloudProvider identifies a supported cloud provider.
type CloudProvider string

const (
	CloudProviderAWS   CloudProvider = "aws"
	CloudProviderGCP   CloudProvider = "gcp"
	CloudProviderAzure CloudProvider = "azure"
)

// PricingTerm describes the commitment level of a price.
type PricingTerm string

const (
	PricingTermOnDemand           PricingTerm = "on_demand"
	PricingTermReserved1Yr        PricingTerm = "reserved_1yr"
	PricingTermReserved3Yr        PricingTerm = "reserved_3yr"
	PricingTermReserved1YrPartial PricingTerm = "reserved_1yr_partial"
	PricingTermReserved1YrAll     PricingTerm = "reserved_1yr_all"
	PricingTermReserved3YrPartial PricingTerm = "reserved_3yr_partial"
	PricingTermReserved3YrAll     PricingTerm = "reserved_3yr_all"
	PricingTermSpot               PricingTerm = "spot"
	PricingTermSavingsPlan        PricingTerm = "savings_plan"
	PricingTermComputeSP          PricingTerm = "compute_savings_plan"
	PricingTermEC2InstanceSP      PricingTerm = "ec2_instance_savings_plan"
	PricingTermSageMakerSP        PricingTerm = "sagemaker_savings_plan"
	PricingTermCUD1Yr             PricingTerm = "cud_1yr"
	PricingTermCUD3Yr             PricingTerm = "cud_3yr"
	PricingTermFlexCUD            PricingTerm = "flex_cud"
	PricingTermSUD                PricingTerm = "sud"
	PricingTermPTU                PricingTerm = "provisioned_throughput_units"
)

// PricingDomain classifies what is being priced.
type PricingDomain string

const (
	PricingDomainCompute           PricingDomain = "compute"
	PricingDomainStorage           PricingDomain = "storage"
	PricingDomainDatabase          PricingDomain = "database"
	PricingDomainContainer         PricingDomain = "container"
	PricingDomainAI                PricingDomain = "ai"
	PricingDomainServerless        PricingDomain = "serverless"
	PricingDomainAnalytics         PricingDomain = "analytics"
	PricingDomainNetwork           PricingDomain = "network"
	PricingDomainObservability     PricingDomain = "observability"
	PricingDomainInterRegionEgress PricingDomain = "inter_region_egress"
)

// PriceUnit describes the unit of a price.
type PriceUnit string

const (
	PriceUnitPerHour      PriceUnit = "per_hour"
	PriceUnitPerMonth     PriceUnit = "per_month"
	PriceUnitPerGBMonth   PriceUnit = "per_gb_month"
	PriceUnitPerGB        PriceUnit = "per_gb"
	PriceUnitPerIOPSMonth PriceUnit = "per_iops_month"
	PriceUnitPerMBPSMonth PriceUnit = "per_mbps_month"
	PriceUnitPerRequest   PriceUnit = "per_request"
	PriceUnitPerGBSecond  PriceUnit = "per_gb_second"
	PriceUnitPerQuery     PriceUnit = "per_query"
	PriceUnitPerUnit      PriceUnit = "per_unit"
)

// PricingSpec is the marker interface for all pricing specification variants.
// The concrete implementations (ComputePricingSpec, StoragePricingSpec, etc.)
// with their discriminated-union UnmarshalJSON are defined by Agent 1A.
type PricingSpec interface {
	GetProvider() CloudProvider
	GetDomain() PricingDomain
	GetService() string
	GetRegion() string
	GetTerm() PricingTerm
	CacheKey() string
}

// NormalizedPrice is the provider-agnostic pricing entry — the core data model.
type NormalizedPrice struct {
	Provider        CloudProvider     `json:"provider"`
	Service         string            `json:"service"`
	SKUID           string            `json:"sku_id"`
	ProductFamily   string            `json:"product_family"`
	Description     string            `json:"description"`
	Region          string            `json:"region"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	PricingTerm     PricingTerm       `json:"pricing_term"`
	PricePerUnit    float64           `json:"price_per_unit"`
	Unit            PriceUnit         `json:"unit"`
	Currency        string            `json:"currency"`
	EffectiveDate   *time.Time        `json:"effective_date,omitempty"`
	FetchedAt       *time.Time        `json:"fetched_at,omitempty"`
	SourceURL       string            `json:"source_url,omitempty"`
	CacheAgeSeconds *int              `json:"cache_age_seconds,omitempty"`
}

// MonthlyCost returns the estimated monthly cost assuming 730 hrs/month.
func (n *NormalizedPrice) MonthlyCost() float64 {
	switch n.Unit { //nolint:exhaustive // unlisted units return raw PricePerUnit as best-effort
	case PriceUnitPerHour:
		return n.PricePerUnit * 730
	case PriceUnitPerMonth:
		return n.PricePerUnit
	default:
		return n.PricePerUnit
	}
}

// HourlyCost returns the estimated hourly cost.
func (n *NormalizedPrice) HourlyCost() float64 {
	switch n.Unit { //nolint:exhaustive // unlisted units return raw PricePerUnit as best-effort
	case PriceUnitPerHour:
		return n.PricePerUnit
	case PriceUnitPerMonth:
		return n.PricePerUnit / 730
	default:
		return n.PricePerUnit
	}
}

// EffectivePrice reflects actual account discounts on top of a base price.
type EffectivePrice struct {
	BasePrice             NormalizedPrice `json:"base_price"`
	EffectivePricePerUnit float64         `json:"effective_price_per_unit"`
	DiscountType          string          `json:"discount_type"`
	DiscountPct           float64         `json:"discount_pct"`
	CommitmentTerm        string          `json:"commitment_term,omitempty"`
	Source                string          `json:"source,omitempty"`
}

// SavingsVsOnDemand returns the per-unit savings compared to on-demand pricing.
func (e *EffectivePrice) SavingsVsOnDemand() float64 {
	return e.BasePrice.PricePerUnit - e.EffectivePricePerUnit
}

// InstanceTypeInfo holds metadata about a compute instance type.
type InstanceTypeInfo struct {
	Provider           CloudProvider `json:"provider"`
	InstanceType       string        `json:"instance_type"`
	VCPU               int           `json:"vcpu"`
	MemoryGB           float64       `json:"memory_gb"`
	GPUCount           int           `json:"gpu_count,omitempty"`
	GPUType            string        `json:"gpu_type,omitempty"`
	NetworkPerformance string        `json:"network_performance,omitempty"`
	Storage            string        `json:"storage,omitempty"`
	Region             string        `json:"region"`
	Available          bool          `json:"available"`
}

// PricingResult is the unified response from GetPrice.
type PricingResult struct {
	PublicPrices     []NormalizedPrice `json:"public_prices"`
	ContractedPrices []NormalizedPrice `json:"contracted_prices,omitempty"`
	EffectivePrice   *EffectivePrice   `json:"effective_price,omitempty"`
	AuthAvailable    bool              `json:"auth_available"`
	Breakdown        map[string]any    `json:"breakdown,omitempty"`
	Note             string            `json:"note,omitempty"`
	Source           string            `json:"source"`
	SchemaVersion    string            `json:"schema_version"`
}

// ProviderCatalog describes what a provider supports and how to call GetPrice.
type ProviderCatalog struct {
	Provider           CloudProvider             `json:"provider"`
	Domains            []PricingDomain           `json:"domains"`
	Services           map[string][]string       `json:"services"`
	SupportedTerms     map[string][]string       `json:"supported_terms"`
	FilterHints        map[string]map[string]any `json:"filter_hints"`
	ExampleInvocations map[string]map[string]any `json:"example_invocations"`
	DecisionMatrix     map[string]string         `json:"decision_matrix,omitempty"`
}

// --------------------------------------------------------------------------
// BasePricingSpec — common fields shared by all PricingSpec variants.
// --------------------------------------------------------------------------

// BasePricingSpec holds fields common to every pricing spec variant.
// Concrete specs embed this struct and implement the PricingSpec interface.
type BasePricingSpec struct {
	Provider      CloudProvider `json:"provider"`
	Domain        PricingDomain `json:"domain"`
	Service       string        `json:"service,omitempty"`
	Region        string        `json:"region"`
	Term          PricingTerm   `json:"term"`
	SchemaVersion string        `json:"schema_version"`
}

// GetProvider implements PricingSpec.
func (b *BasePricingSpec) GetProvider() CloudProvider { return b.Provider }

// GetDomain implements PricingSpec.
func (b *BasePricingSpec) GetDomain() PricingDomain { return b.Domain }

// GetService implements PricingSpec.
func (b *BasePricingSpec) GetService() string { return b.Service }

// GetRegion implements PricingSpec.
func (b *BasePricingSpec) GetRegion() string { return b.Region }

// GetTerm implements PricingSpec.
func (b *BasePricingSpec) GetTerm() PricingTerm { return b.Term }

// baseCacheKey returns the shared cache key prefix for all spec variants.
// Named baseCacheKey (not CacheKey) so each variant must define its own
// CacheKey() — a forgotten override is a compile error on the interface.
func (b *BasePricingSpec) baseCacheKey() string {
	parts := []string{
		string(b.Provider),
		string(b.Domain),
		b.Service,
		b.Region,
		string(b.Term),
	}
	return strings.Join(parts, ":")
}

// defaultBase returns a BasePricingSpec with all defaults pre-populated,
// mirroring BasePricingSpec in Python.
func defaultBase() BasePricingSpec {
	return BasePricingSpec{
		Term:          PricingTermOnDemand,
		SchemaVersion: "1",
	}
}

// --------------------------------------------------------------------------
// Concrete PricingSpec variants — one per PricingDomain.
// --------------------------------------------------------------------------

// ComputePricingSpec prices VMs, bare metal, and Fargate tasks.
type ComputePricingSpec struct {
	BasePricingSpec
	ResourceType  string   `json:"resource_type,omitempty"`
	OS            string   `json:"os"`
	VCPU          *float64 `json:"vcpu,omitempty"`
	MemoryGB      *float64 `json:"memory_gb,omitempty"`
	HoursPerMonth float64  `json:"hours_per_month"`
}

// Compile-time interface guard.
var _ PricingSpec = (*ComputePricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *ComputePricingSpec) CacheKey() string {
	vcpu := ""
	if s.VCPU != nil {
		vcpu = fmt.Sprintf("%v", *s.VCPU)
	}
	mem := ""
	if s.MemoryGB != nil {
		mem = fmt.Sprintf("%v", *s.MemoryGB)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%s", s.baseCacheKey(), s.ResourceType, s.OS, vcpu, mem)
}

// UnmarshalJSON handles the instance_type → resource_type alias (mirrors
// the Python model_validator) in addition to standard field decoding.
func (s *ComputePricingSpec) UnmarshalJSON(data []byte) error {
	// Pre-populate defaults before unmarshal so absent fields keep them.
	*s = ComputePricingSpec{
		BasePricingSpec: defaultBase(),
		OS:              "Linux",
		HoursPerMonth:   730.0,
	}

	// Use an alias to avoid infinite recursion.
	type alias ComputePricingSpec
	aux := struct {
		InstanceType string `json:"instance_type"`
		*alias
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Apply alias: instance_type wins if resource_type was not set.
	if aux.InstanceType != "" && s.ResourceType == "" {
		s.ResourceType = aux.InstanceType
	}
	return nil
}

// StoragePricingSpec prices block and object storage.
type StoragePricingSpec struct {
	BasePricingSpec
	StorageType string   `json:"storage_type"`
	SizeGB      *float64 `json:"size_gb,omitempty"`
	IOPS        *int     `json:"iops,omitempty"`
}

var _ PricingSpec = (*StoragePricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *StoragePricingSpec) CacheKey() string {
	sizeGB := ""
	if s.SizeGB != nil {
		sizeGB = fmt.Sprintf("%v", *s.SizeGB)
	}
	iops := ""
	if s.IOPS != nil {
		iops = fmt.Sprintf("%v", *s.IOPS)
	}
	return fmt.Sprintf("%s:%s:%s:%s", s.baseCacheKey(), s.StorageType, sizeGB, iops)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *StoragePricingSpec) UnmarshalJSON(data []byte) error {
	*s = StoragePricingSpec{
		BasePricingSpec: defaultBase(),
		StorageType:     "gp3",
	}
	type alias StoragePricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// DatabasePricingSpec prices managed relational DBs and in-memory caches.
type DatabasePricingSpec struct {
	BasePricingSpec
	ResourceType  string   `json:"resource_type"`
	Engine        string   `json:"engine"`
	Deployment    string   `json:"deployment"`
	StorageGB     *float64 `json:"storage_gb,omitempty"`
	CapacityGB    *float64 `json:"capacity_gb,omitempty"`
	HoursPerMonth float64  `json:"hours_per_month"`
}

var _ PricingSpec = (*DatabasePricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *DatabasePricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%s:%s:%s", s.baseCacheKey(), s.ResourceType, s.Engine, s.Deployment)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *DatabasePricingSpec) UnmarshalJSON(data []byte) error {
	*s = DatabasePricingSpec{
		BasePricingSpec: defaultBase(),
		ResourceType:    "",
		Engine:          "MySQL",
		Deployment:      "single-az",
		HoursPerMonth:   730.0,
	}
	type alias DatabasePricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// ContainerPricingSpec prices managed Kubernetes / container control planes.
type ContainerPricingSpec struct {
	BasePricingSpec
	Mode          string   `json:"mode"`
	NodeType      string   `json:"node_type,omitempty"`
	NodeCount     int      `json:"node_count"`
	VCPU          *float64 `json:"vcpu,omitempty"`
	MemoryGB      *float64 `json:"memory_gb,omitempty"`
	HoursPerMonth float64  `json:"hours_per_month"`
}

var _ PricingSpec = (*ContainerPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *ContainerPricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%s:%s:%d", s.baseCacheKey(), s.Mode, s.NodeType, s.NodeCount)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *ContainerPricingSpec) UnmarshalJSON(data []byte) error {
	*s = ContainerPricingSpec{
		BasePricingSpec: defaultBase(),
		Mode:            "standard",
		NodeCount:       3,
		HoursPerMonth:   730.0,
	}
	type alias ContainerPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// AiPricingSpec prices AI/ML inference and training.
type AiPricingSpec struct {
	BasePricingSpec
	Model         string   `json:"model,omitempty"`
	MachineType   string   `json:"machine_type,omitempty"`
	Task          string   `json:"task"`
	InputTokens   *int     `json:"input_tokens,omitempty"`
	OutputTokens  *int     `json:"output_tokens,omitempty"`
	TrainingHours *float64 `json:"training_hours,omitempty"`
	Mode          string   `json:"mode"`
}

var _ PricingSpec = (*AiPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *AiPricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", s.baseCacheKey(), s.Model, s.MachineType, s.Task, s.Mode)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *AiPricingSpec) UnmarshalJSON(data []byte) error {
	*s = AiPricingSpec{
		BasePricingSpec: defaultBase(),
		Task:            "inference",
		Mode:            "on_demand",
	}
	type alias AiPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// ServerlessPricingSpec prices function-as-a-service offerings.
type ServerlessPricingSpec struct {
	BasePricingSpec
	GBSeconds        *float64 `json:"gb_seconds,omitempty"`
	RequestsMillions *float64 `json:"requests_millions,omitempty"`
	HoursPerMonth    *float64 `json:"hours_per_month,omitempty"`
}

var _ PricingSpec = (*ServerlessPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *ServerlessPricingSpec) CacheKey() string {
	gb := ""
	if s.GBSeconds != nil {
		gb = fmt.Sprintf("%v", *s.GBSeconds)
	}
	req := ""
	if s.RequestsMillions != nil {
		req = fmt.Sprintf("%v", *s.RequestsMillions)
	}
	return fmt.Sprintf("%s:%s:%s", s.baseCacheKey(), gb, req)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *ServerlessPricingSpec) UnmarshalJSON(data []byte) error {
	*s = ServerlessPricingSpec{
		BasePricingSpec: defaultBase(),
	}
	type alias ServerlessPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// AnalyticsPricingSpec prices data warehouse and analytics query engines.
type AnalyticsPricingSpec struct {
	BasePricingSpec
	QueryTB           *float64 `json:"query_tb,omitempty"`
	ActiveStorageGB   *float64 `json:"active_storage_gb,omitempty"`
	LongtermStorageGB *float64 `json:"longterm_storage_gb,omitempty"`
	StreamingGB       *float64 `json:"streaming_gb,omitempty"`
}

var _ PricingSpec = (*AnalyticsPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *AnalyticsPricingSpec) CacheKey() string {
	qtb := ""
	if s.QueryTB != nil {
		qtb = fmt.Sprintf("%v", *s.QueryTB)
	}
	asg := ""
	if s.ActiveStorageGB != nil {
		asg = fmt.Sprintf("%v", *s.ActiveStorageGB)
	}
	return fmt.Sprintf("%s:%s:%s", s.baseCacheKey(), qtb, asg)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *AnalyticsPricingSpec) UnmarshalJSON(data []byte) error {
	*s = AnalyticsPricingSpec{
		BasePricingSpec: defaultBase(),
	}
	type alias AnalyticsPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// NetworkPricingSpec prices load balancers, CDN, NAT, egress, and WAF.
type NetworkPricingSpec struct {
	BasePricingSpec
	LBType                  string  `json:"lb_type,omitempty"`
	RuleCount               int     `json:"rule_count"`
	DataGB                  float64 `json:"data_gb"`
	GatewayCount            int     `json:"gateway_count"`
	EgressGB                float64 `json:"egress_gb"`
	CacheFillGB             float64 `json:"cache_fill_gb"`
	PolicyCount             int     `json:"policy_count"`
	MonthlyRequestsMillions float64 `json:"monthly_requests_millions"`
	HoursPerMonth           float64 `json:"hours_per_month"`
	// Egress-specific fields
	SourceRegion      string  `json:"source_region"`
	DestinationType   string  `json:"destination_type"`
	DestinationRegion string  `json:"destination_region"`
	DataGBPerMonth    float64 `json:"data_gb_per_month"`
	NetworkTier       string  `json:"network_tier"`
}

var _ PricingSpec = (*NetworkPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *NetworkPricingSpec) CacheKey() string {
	if s.Service == "egress" {
		return fmt.Sprintf("%s:%s:%s:%s:%v:%s",
			s.baseCacheKey(),
			s.SourceRegion,
			s.DestinationType,
			s.DestinationRegion,
			s.DataGBPerMonth,
			s.NetworkTier,
		)
	}
	return fmt.Sprintf("%s:%s:%d:%d", s.baseCacheKey(), s.LBType, s.RuleCount, s.GatewayCount)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *NetworkPricingSpec) UnmarshalJSON(data []byte) error {
	*s = NetworkPricingSpec{
		BasePricingSpec: defaultBase(),
		RuleCount:       1,
		GatewayCount:    1,
		PolicyCount:     1,
		HoursPerMonth:   730.0,
		DestinationType: "internet",
		NetworkTier:     "premium",
	}
	type alias NetworkPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// ObservabilityPricingSpec prices metrics, logs, and traces.
type ObservabilityPricingSpec struct {
	BasePricingSpec
	IngestionMiB float64 `json:"ingestion_mib"`
	MetricsCount int     `json:"metrics_count"`
	LogGB        float64 `json:"log_gb"`
}

var _ PricingSpec = (*ObservabilityPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *ObservabilityPricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%v:%d:%v", s.baseCacheKey(), s.IngestionMiB, s.MetricsCount, s.LogGB)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *ObservabilityPricingSpec) UnmarshalJSON(data []byte) error {
	*s = ObservabilityPricingSpec{
		BasePricingSpec: defaultBase(),
	}
	type alias ObservabilityPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// EgressPricingSpec prices region-to-region data transfer.
type EgressPricingSpec struct {
	BasePricingSpec
	SourceRegion string  `json:"source_region"`
	DestRegion   string  `json:"dest_region"`
	DataGB       float64 `json:"data_gb"`
}

var _ PricingSpec = (*EgressPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *EgressPricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%s:%s", s.baseCacheKey(), s.SourceRegion, s.DestRegion)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *EgressPricingSpec) UnmarshalJSON(data []byte) error {
	*s = EgressPricingSpec{
		BasePricingSpec: defaultBase(),
		DataGB:          1.0,
	}
	type alias EgressPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// --------------------------------------------------------------------------
// Discriminated-union dispatcher
// --------------------------------------------------------------------------

// domainPeek is used to peek the "domain" field from a JSON object without
// fully decoding it.
type domainPeek struct {
	Domain PricingDomain `json:"domain"`
}

// UnmarshalPricingSpec decodes a JSON object into the correct PricingSpec
// variant by peeking the "domain" field first (two-pass decode). It mirrors
// Pydantic's discriminated-union behaviour on the PricingSpec type in models.py.
//
// Supported domains: compute, storage, database, container, ai, serverless,
// analytics, network, observability, inter_region_egress.
func UnmarshalPricingSpec(data []byte) (PricingSpec, error) {
	var peek domainPeek
	if err := json.Unmarshal(data, &peek); err != nil {
		return nil, fmt.Errorf("models: cannot peek domain field: %w", err)
	}

	switch peek.Domain {
	case PricingDomainCompute:
		var s ComputePricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: ComputePricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainStorage:
		var s StoragePricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: StoragePricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainDatabase:
		var s DatabasePricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: DatabasePricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainContainer:
		var s ContainerPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: ContainerPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainAI:
		var s AiPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: AiPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainServerless:
		var s ServerlessPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: ServerlessPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainAnalytics:
		var s AnalyticsPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: AnalyticsPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainNetwork:
		var s NetworkPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: NetworkPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainObservability:
		var s ObservabilityPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: ObservabilityPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainInterRegionEgress:
		var s EgressPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: EgressPricingSpec: %w", err)
		}
		return &s, nil

	default:
		return nil, fmt.Errorf("models: unknown pricing domain %q", peek.Domain)
	}
}
