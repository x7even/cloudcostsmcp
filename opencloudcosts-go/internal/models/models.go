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
	PricingDomainSecurity          PricingDomain = "security"
	PricingDomainDNS               PricingDomain = "dns"
	PricingDomainMessaging         PricingDomain = "messaging"
	PricingDomainNoSQL             PricingDomain = "nosql"
)

// PriceUnit describes the unit of a price.
type PriceUnit string

const (
	PriceUnitPerHour            PriceUnit = "per_hour"
	PriceUnitPerMonth           PriceUnit = "per_month"
	PriceUnitPerGBMonth         PriceUnit = "per_gb_month"
	PriceUnitPerGB              PriceUnit = "per_gb"
	PriceUnitPerIOPSMonth       PriceUnit = "per_iops_month"
	PriceUnitPerMBPSMonth       PriceUnit = "per_mbps_month"
	PriceUnitPerRequest         PriceUnit = "per_request"
	PriceUnitPerGBSecond        PriceUnit = "per_gb_second"
	PriceUnitPerQuery           PriceUnit = "per_query"
	PriceUnitPerUnit            PriceUnit = "per_unit"
	PriceUnitPerKeyVersionMonth PriceUnit = "per_key_version_month"
	PriceUnitPerOperation       PriceUnit = "per_operation"
	PriceUnitPerZoneMonth       PriceUnit = "per_zone_month"
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
	// Savings Plan fields — only relevant when Term is compute_savings_plan or ec2_instance_savings_plan.
	// PaymentOption selects the SP payment structure: 'No Upfront' | 'Partial Upfront' | 'All Upfront'.
	// Defaults to 'No Upfront'.
	PaymentOption *string `json:"payment_option,omitempty"`
	// CommitmentYears is the SP commitment duration in years: 1 or 3. Defaults to 1.
	CommitmentYears *int `json:"commitment_years,omitempty"`
	// EDPDiscountPct is a fractional EDP/PPA discount (0.0–1.0) applied on top of the SP rate.
	// EDP is a confidential negotiated rate; supply your contract percentage to calculate
	// the adjusted effective rate. Pointer so that absent means "no EDP" (not 0%).
	EDPDiscountPct *float64 `json:"edp_discount_pct,omitempty"`
}

// Compile-time interface guard.
var _ PricingSpec = (*ComputePricingSpec)(nil)

// CacheKey implements PricingSpec.
// Note: EDPDiscountPct is intentionally excluded — the public SP rate is cached once
// and the EDP multiplier is applied after retrieval, so different EDP values share one cache entry.
func (s *ComputePricingSpec) CacheKey() string {
	vcpu := ""
	if s.VCPU != nil {
		vcpu = fmt.Sprintf("%v", *s.VCPU)
	}
	mem := ""
	if s.MemoryGB != nil {
		mem = fmt.Sprintf("%v", *s.MemoryGB)
	}
	paymentOption := ""
	if s.PaymentOption != nil {
		paymentOption = *s.PaymentOption
	}
	commitmentYears := ""
	if s.CommitmentYears != nil {
		commitmentYears = fmt.Sprintf("%d", *s.CommitmentYears)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%s:%s:%s", s.baseCacheKey(), s.ResourceType, s.OS, vcpu, mem, paymentOption, commitmentYears)
}

// UnmarshalJSON handles the instance_type → resource_type alias (mirrors
// the Python model_validator) in addition to standard field decoding.
// Defaults for SP fields: PaymentOption='No Upfront', CommitmentYears=1.
func (s *ComputePricingSpec) UnmarshalJSON(data []byte) error {
	defaultPaymentOption := "No Upfront"
	defaultCommitmentYears := 1
	// Pre-populate defaults before unmarshal so absent fields keep them.
	*s = ComputePricingSpec{
		BasePricingSpec: defaultBase(),
		OS:              "Linux",
		HoursPerMonth:   730.0,
		PaymentOption:   &defaultPaymentOption,
		CommitmentYears: &defaultCommitmentYears,
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
	StorageType    string   `json:"storage_type"`
	SizeGB         *float64 `json:"size_gb,omitempty"`
	IOPS           *int     `json:"iops,omitempty"`
	ThroughputMBPS *float64 `json:"throughput_mbps,omitempty"`
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
	throughput := ""
	if s.ThroughputMBPS != nil {
		throughput = fmt.Sprintf("%v", *s.ThroughputMBPS)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%s", s.baseCacheKey(), s.StorageType, sizeGB, iops, throughput)
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

// KMSPricingSpec prices Cloud KMS / key-management key-versions and
// cryptographic operations (domain=security, service=kms).
//
// Cloud KMS bills along two independent dimensions — active key-version-months
// and cryptographic operation counts — bifurcated by protection level
// (key_type) and, for HSM only, by algorithm (algorithm). Pricing is
// region-invariant (scope="global" on every returned NormalizedPrice); Region
// is accepted but ignored by the GCP provider.
type KMSPricingSpec struct {
	BasePricingSpec
	// KeyType selects the protection level: "software" (default) | "hsm" | "external".
	KeyType string `json:"key_type,omitempty"`
	// Algorithm selects the key algorithm, which only affects price for key_type="hsm":
	// "symmetric" (default) | "mac" | "asymmetric-rsa2048" | "asymmetric-rsa3072" |
	// "asymmetric-rsa4096" | "asymmetric-ec" | "asymmetric-pkcs1v15" (limited
	// availability: asia-south1/asia-south2 only).
	Algorithm string `json:"algorithm,omitempty"`
	// Unit selects the billing dimension: "key_version_month" (default) |
	// "crypto_operations" | "random_bytes".
	Unit string `json:"unit,omitempty"`
	// Autokey, when true, prices the Cloud KMS Autokey SKU variant, which
	// includes a monthly free allowance (100 key versions or 10,000
	// operations) before the paid rate applies. The returned PricePerUnit is
	// always the paid rate; the free allowance is reported in Breakdown.
	Autokey bool `json:"autokey,omitempty"`
	// KeyVersions is the optional key-version count for a monthly cost estimate
	// (unit="key_version_month").
	KeyVersions *float64 `json:"key_versions,omitempty"`
	// OperationsPerMonth is the optional monthly operation count for a cost
	// estimate (unit="crypto_operations" or "random_bytes").
	OperationsPerMonth *float64 `json:"operations_per_month,omitempty"`
}

var _ PricingSpec = (*KMSPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *KMSPricingSpec) CacheKey() string {
	return fmt.Sprintf("%s:%s:%s:%s:%v", s.baseCacheKey(), s.KeyType, s.Algorithm, s.Unit, s.Autokey)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *KMSPricingSpec) UnmarshalJSON(data []byte) error {
	*s = KMSPricingSpec{
		BasePricingSpec: defaultBase(),
		KeyType:         "software",
		Algorithm:       "symmetric",
		Unit:            "key_version_month",
	}
	type alias KMSPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// DNSPricingSpec prices GCP Cloud DNS managed zones and DNS queries
// (domain=dns, service=cloud_dns).
//
// Cloud DNS bills along two independent, region-invariant dimensions:
//   - a per-zone-month charge, volume-discounted by the total managed-zone
//     count (zones 1-25, 26-10,000, 10,001+); and
//   - a per-query charge for standard DNS resolution (port 53) queries,
//     volume-discounted above 1,000,000,000 queries/month.
//
// zone_type does NOT change price: public, private, forwarding, and (by
// inference) peering zones all resolve to the single shared ManagedZone tier
// ladder (verified live against the GCP Cloud Billing Catalog API, issue
// #78). ZoneType is retained as a validated, informational field only —
// mirroring the role KeyType plays in KMSPricingSpec — not because it
// selects a different rate.
//
// "Routing policy queries" ($0.70/$0.35 per million) are a real, documented
// GCP charge but have no catalog SKU under this service ID; they are out of
// scope for this spec.
//
// Pricing is region-invariant (scope="global" on every returned
// NormalizedPrice); Region is accepted but ignored by the GCP provider.
type DNSPricingSpec struct {
	BasePricingSpec
	// ZoneType is informational only — every zone type shares the single
	// ManagedZone tier ladder, so this field never changes price.
	// "public" (default) | "private" | "forwarding" | "peering".
	ZoneType string `json:"zone_type,omitempty"`
	// ZoneCount is the optional total managed-zone count (across all zone
	// types) for a monthly cost estimate.
	ZoneCount *float64 `json:"zone_count,omitempty"`
	// QueriesPerMonth is the optional monthly DNS query volume (port 53) for
	// a monthly cost estimate.
	QueriesPerMonth *float64 `json:"queries_per_month,omitempty"`
}

var _ PricingSpec = (*DNSPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *DNSPricingSpec) CacheKey() string {
	zoneCount := ""
	if s.ZoneCount != nil {
		zoneCount = fmt.Sprintf("%v", *s.ZoneCount)
	}
	queries := ""
	if s.QueriesPerMonth != nil {
		queries = fmt.Sprintf("%v", *s.QueriesPerMonth)
	}
	return fmt.Sprintf("%s:%s:%s:%s", s.baseCacheKey(), s.ZoneType, zoneCount, queries)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *DNSPricingSpec) UnmarshalJSON(data []byte) error {
	*s = DNSPricingSpec{
		BasePricingSpec: defaultBase(),
		ZoneType:        "public",
	}
	type alias DNSPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// PubSubPricingSpec prices GCP Cloud Pub/Sub message throughput and message
// storage/retention (domain=messaging, service=pubsub).
//
// Cloud Pub/Sub bills along two independent, region-invariant dimensions:
//   - a per-GiB throughput charge for message delivery, bifurcated by
//     destination — Basic delivery includes a 10 GiB/month free allowance
//     before the paid rate applies, while BigQuery/Cloud-Storage/Bigtable
//     direct-write "ingestion" destinations and cross-cloud/on-prem "import"
//     destinations (Kinesis, Azure Event Hubs, AWS MSK, Confluent Cloud) and
//     Single Message Transform (SMT) throughput (UDF and AI-inference) are
//     each billed a flat rate with no free tier; and
//   - a per-GiB-month storage charge for retained message backlog (topics,
//     subscriptions, snapshots) and retained acknowledged messages — all four
//     of which share one identical rate.
//
// Verified live against the GCP Cloud Billing Catalog API (service
// A1E8-BE35-7EBC, "Cloud Pub/Sub", issue #79): all 77 SKUs under this service
// are tagged serviceRegions=["global"] with geoTaxonomy.type either "GLOBAL"
// or absent (never "REGIONAL") — pricing is genuinely region-invariant, not
// merely a documentation convention. This confirms (rather than merely
// assumes) the issue's "candidate for scope:global" framing.
//
// Out of scope for this spec (deliberately, not gaps):
//   - Pub/Sub Lite (a distinct service, serviceId 3A1B-66C4-2BAE) — different
//     pricing model (provisioned capacity + storage), not covered here.
//   - The 61 Network/egress SKUs under this same service ID. They are also
//     tagged serviceRegions=["global"] at the API level, but encode actual
//     geography as continent-pair corridors inside the SKU description
//     string (not a flat global rate) — a materially more complex shape
//     that is deferred to a future, dedicated egress spec.
//   - Schema Registry, seek/replay, filtered messages, and dead-letter
//     topics/ordering keys are confirmed NOT billed as separate line items
//     (no corresponding SKU exists) — these are not gaps in this
//     implementation, they are genuinely free.
//
// Pricing is region-invariant (scope="global" on every returned
// NormalizedPrice); Region is accepted but ignored by the GCP provider.
type PubSubPricingSpec struct {
	BasePricingSpec
	// Destination selects the message-delivery throughput rate and DOES
	// affect price: "basic" (default, 10 GiB/month free then paid) |
	// "bigquery" | "cloud_storage_export" | "bigtable" | "kinesis_import" |
	// "cloud_storage_import" | "azure_event_hubs_import" | "aws_msk_import" |
	// "confluent_cloud_import" | "smt_udf" | "smt_ai_inference".
	Destination string `json:"destination,omitempty"`
	// StorageType is informational only — topic backlog, subscription
	// backlog, retained acknowledged messages, and snapshot backlog all
	// share the single storage rate, so this field never changes price.
	// "topic_backlog" (default) | "subscription_backlog" |
	// "retained_acked_messages" | "snapshot_backlog".
	StorageType string `json:"storage_type,omitempty"`
	// ThroughputGBPerMonth is the optional monthly message-throughput volume
	// (GiB) for a monthly cost estimate.
	ThroughputGBPerMonth *float64 `json:"throughput_gb_per_month,omitempty"`
	// StorageGB is the optional average monthly retained message backlog /
	// retained-acknowledged-message volume (GiB) for a monthly cost estimate.
	StorageGB *float64 `json:"storage_gb,omitempty"`
}

var _ PricingSpec = (*PubSubPricingSpec)(nil)

// CacheKey implements PricingSpec.
func (s *PubSubPricingSpec) CacheKey() string {
	throughput := ""
	if s.ThroughputGBPerMonth != nil {
		throughput = fmt.Sprintf("%v", *s.ThroughputGBPerMonth)
	}
	storage := ""
	if s.StorageGB != nil {
		storage = fmt.Sprintf("%v", *s.StorageGB)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%s", s.baseCacheKey(), s.Destination, s.StorageType, throughput, storage)
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *PubSubPricingSpec) UnmarshalJSON(data []byte) error {
	*s = PubSubPricingSpec{
		BasePricingSpec: defaultBase(),
		Destination:     "basic",
		StorageType:     "topic_backlog",
	}
	type alias PubSubPricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// FirestorePricingSpec prices GCP Cloud Firestore storage, read/write/delete
// operations, TTL deletes, point-in-time-recovery (PITR) storage, zonal
// backup storage, backup restore operations, and database clone operations
// (domain=nosql, service=firestore).
//
// Unlike Cloud DNS/Cloud KMS/Cloud Pub/Sub (all genuinely region-invariant,
// scope="global"), Cloud Firestore is genuinely per-region priced: verified
// live against the GCP Cloud Billing Catalog API (service EE2C-7FAC-5E08,
// "Cloud Firestore", 2072 SKUs total, fully paginated — see issue #80).
// serviceRegions is misleadingly ["global"] on every Firestore SKU regardless
// of actual region-specificity; the authoritative region signal is
// geoTaxonomy.type ("REGIONAL" | "MULTI_REGIONAL" | "GLOBAL") and
// geoTaxonomy.regions. Region is therefore a real, rate-selecting dimension
// on this spec (unlike DNSPricingSpec/PubSubPricingSpec, where Region is
// accepted but ignored) — see gcp_firestore.go for the region-keyed rate
// cache and matching strategy.
//
// Billing dimensions:
//   - StorageGB: average monthly stored data, $/GiB-month, with a genuine
//     (verified live) free allowance.
//   - ReadsPerMonth / WritesPerMonth / DeletesPerMonth: entity read/write/
//     delete operation counts, each with a genuine (verified live) free
//     allowance.
//   - TTLDeletesPerMonth: TTL (time-to-live) delete operation count. No
//     genuine free allowance despite a "(with free tier)" SKU description —
//     verified live that both description variants carry byte-identical
//     tiered rates.
//   - PITRStorageGB: point-in-time-recovery storage, $/GiB-month. No genuine
//     free allowance (same "(with free tier)" naming caveat as above).
//   - ZonalBackupStorageGB: zonal backup storage, $/GiB-month. No genuine
//     free allowance.
//   - RestoreGB: backup restore operation volume, $/GiB restored. No genuine
//     free allowance.
//   - CloneGB: database clone operation volume, $/GiB cloned. No genuine
//     free allowance. (Verified live under the Enterprise/Datastore
//     resourceGroup "DatastoreOps", but is itself a Standard-edition SKU, not
//     an Enterprise-edition one — see gcp_firestore.go.)
//
// Explicitly out of scope (deliberately, not gaps):
//   - Firestore Enterprise edition (MongoDB-compatible) — a materially
//     different pricing model (resourceGroup DatastoreOps/DatastoreBandwidth,
//     or any description containing "Enterprise") is excluded entirely.
//   - The 4 GLOBAL "CUD metadata" SKUs found under the FirestoreReadOps
//     resourceGroup — these are not genuine regional read-operation rates.
//   - Firestore/Datastore network bandwidth and egress SKUs (resourceGroup
//     FirestoreBandwidth / DatastoreBandwidth) — deferred to a future,
//     dedicated egress spec, same precedent as Cloud Pub/Sub's egress SKUs
//     (#79).
//   - "Small operations" (resourceGroup FirestoreSmallOps) are confirmed
//     live to always be $0 regardless of volume — genuinely free, not a gap.
type FirestorePricingSpec struct {
	BasePricingSpec
	// StorageGB is the optional average monthly stored data volume (GiB) for
	// a monthly cost estimate.
	StorageGB *float64 `json:"storage_gb,omitempty"`
	// ReadsPerMonth is the optional monthly entity read operation count for a
	// monthly cost estimate.
	ReadsPerMonth *float64 `json:"reads_per_month,omitempty"`
	// WritesPerMonth is the optional monthly entity write (put) operation
	// count for a monthly cost estimate.
	WritesPerMonth *float64 `json:"writes_per_month,omitempty"`
	// DeletesPerMonth is the optional monthly entity delete operation count
	// for a monthly cost estimate.
	DeletesPerMonth *float64 `json:"deletes_per_month,omitempty"`
	// TTLDeletesPerMonth is the optional monthly TTL delete operation count
	// for a monthly cost estimate. No genuine free tier — see type doc.
	TTLDeletesPerMonth *float64 `json:"ttl_deletes_per_month,omitempty"`
	// PITRStorageGB is the optional average monthly point-in-time-recovery
	// storage volume (GiB) for a monthly cost estimate. No genuine free
	// tier — see type doc.
	PITRStorageGB *float64 `json:"pitr_storage_gb,omitempty"`
	// ZonalBackupStorageGB is the optional average monthly zonal backup
	// storage volume (GiB) for a monthly cost estimate. No genuine free
	// tier — see type doc.
	ZonalBackupStorageGB *float64 `json:"zonal_backup_storage_gb,omitempty"`
	// RestoreGB is the optional monthly backup restore volume (GiB) for a
	// monthly cost estimate. No genuine free tier — see type doc.
	RestoreGB *float64 `json:"restore_gb,omitempty"`
	// CloneGB is the optional monthly database clone volume (GiB) for a
	// monthly cost estimate. No genuine free tier — see type doc.
	CloneGB *float64 `json:"clone_gb,omitempty"`
}

var _ PricingSpec = (*FirestorePricingSpec)(nil)

// firestoreCacheKeyPart formats an optional *float64 for CacheKey, mirroring
// the inline pattern used by DNSPricingSpec.CacheKey/PubSubPricingSpec.CacheKey.
func firestoreCacheKeyPart(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", *v)
}

// CacheKey implements PricingSpec. Region is already part of baseCacheKey(),
// which (unlike DNSPricingSpec/PubSubPricingSpec, where region is accepted
// but ignored by the provider) actually matters here: Cloud Firestore is
// genuinely per-region priced, so two specs differing only by Region must
// never collide on the same cache entry.
func (s *FirestorePricingSpec) CacheKey() string {
	parts := []string{
		s.baseCacheKey(),
		firestoreCacheKeyPart(s.StorageGB),
		firestoreCacheKeyPart(s.ReadsPerMonth),
		firestoreCacheKeyPart(s.WritesPerMonth),
		firestoreCacheKeyPart(s.DeletesPerMonth),
		firestoreCacheKeyPart(s.TTLDeletesPerMonth),
		firestoreCacheKeyPart(s.PITRStorageGB),
		firestoreCacheKeyPart(s.ZonalBackupStorageGB),
		firestoreCacheKeyPart(s.RestoreGB),
		firestoreCacheKeyPart(s.CloneGB),
	}
	return strings.Join(parts, ":")
}

// UnmarshalJSON pre-populates defaults then decodes.
func (s *FirestorePricingSpec) UnmarshalJSON(data []byte) error {
	*s = FirestorePricingSpec{
		BasePricingSpec: defaultBase(),
	}
	type alias FirestorePricingSpec
	return json.Unmarshal(data, (*alias)(s))
}

// NormalizePaymentOption returns the canonical SP payment option string or an
// error for unknown values. The SP JSON uses these exact case-sensitive strings.
func NormalizePaymentOption(s string) (string, error) {
	switch s {
	case "No Upfront", "no upfront", "no-upfront", "none":
		return "No Upfront", nil
	case "Partial Upfront", "partial upfront", "partial-upfront", "partial":
		return "Partial Upfront", nil
	case "All Upfront", "all upfront", "all-upfront", "all":
		return "All Upfront", nil
	default:
		return "", fmt.Errorf("models: unknown payment option %q: must be 'No Upfront', 'Partial Upfront', or 'All Upfront'", s)
	}
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
// analytics, network, observability, inter_region_egress, security, dns,
// messaging, nosql.
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

	case PricingDomainSecurity:
		var s KMSPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: KMSPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainDNS:
		var s DNSPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: DNSPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainMessaging:
		var s PubSubPricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: PubSubPricingSpec: %w", err)
		}
		return &s, nil

	case PricingDomainNoSQL:
		var s FirestorePricingSpec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("models: FirestorePricingSpec: %w", err)
		}
		return &s, nil

	default:
		return nil, fmt.Errorf("models: unknown pricing domain %q", peek.Domain)
	}
}
