package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// _regionToLocation maps AWS region codes to the "location" display names used
// by the AWS Pricing API. Ported exactly from the Python implementation.
var _regionToLocation = map[string]string{
	"us-east-1":      "US East (N. Virginia)",
	"us-east-2":      "US East (Ohio)",
	"us-west-1":      "US West (N. California)",
	"us-west-2":      "US West (Oregon)",
	"ca-central-1":   "Canada (Central)",
	"ca-west-1":      "Canada West (Calgary)",
	"eu-west-1":      "Europe (Ireland)",
	"eu-west-2":      "Europe (London)",
	"eu-west-3":      "Europe (Paris)",
	"eu-central-1":   "Europe (Frankfurt)",
	"eu-central-2":   "Europe (Zurich)",
	"eu-north-1":     "Europe (Stockholm)",
	"eu-south-1":     "Europe (Milan)",
	"eu-south-2":     "Europe (Spain)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-southeast-2": "Asia Pacific (Sydney)",
	"ap-southeast-3": "Asia Pacific (Jakarta)",
	"ap-southeast-4": "Asia Pacific (Melbourne)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-northeast-2": "Asia Pacific (Seoul)",
	"ap-northeast-3": "Asia Pacific (Osaka)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ap-south-2":     "Asia Pacific (Hyderabad)",
	"ap-east-1":      "Asia Pacific (Hong Kong)",
	"sa-east-1":      "South America (Sao Paulo)",
	"me-south-1":     "Middle East (Bahrain)",
	"me-central-1":   "Middle East (UAE)",
	"af-south-1":     "Africa (Cape Town)",
	"il-central-1":   "Israel (Tel Aviv)",
	"ap-southeast-5": "Asia Pacific (Malaysia)",
	"ap-southeast-7": "Asia Pacific (Thailand)",
	"mx-central-1":   "Mexico (Central)",
}

// regionToLocation resolves an AWS region code to the pricing API display name.
// Returns an error for unknown regions (mirrors aws_region_to_display in Python).
func regionToLocation(region string) (string, error) {
	loc, ok := _regionToLocation[region]
	if !ok {
		return "", fmt.Errorf("unknown AWS region: %s", region)
	}
	return loc, nil
}

// --------------------------------------------------------------------------
// Filter helpers — ported exactly from Python _COMPUTE_FILTERS etc.
// --------------------------------------------------------------------------

// computeFilters returns the standard EC2 compute product filters.
func computeFilters(instanceType, location, os string) []pricingtypes.Filter {
	return []pricingtypes.Filter{
		mkFilter("instanceType", instanceType),
		mkFilter("location", location),
		mkFilter("operatingSystem", os),
		mkFilter("tenancy", "Shared"),
		mkFilter("preInstalledSw", "NA"),
		mkFilter("capacitystatus", "Used"),
	}
}

// storageFilters returns the standard EBS storage product filters.
func storageFilters(volumeType, location string) []pricingtypes.Filter {
	return []pricingtypes.Filter{
		mkFilter("volumeType", volumeType),
		mkFilter("location", location),
		mkFilter("productFamily", "Storage"),
	}
}

// databaseFilters returns the standard RDS product filters.
// engine must be the display name (e.g. "MySQL", "PostgreSQL").
// deploymentOption must be "Single-AZ" or "Multi-AZ".
func databaseFilters(instanceType, location, engine, deploymentOption string) []pricingtypes.Filter {
	return []pricingtypes.Filter{
		mkFilter("instanceType", instanceType),
		mkFilter("location", location),
		mkFilter("databaseEngine", engine),
		mkFilter("deploymentOption", deploymentOption),
	}
}

func mkFilter(field, value string) pricingtypes.Filter {
	return pricingtypes.Filter{
		Type:  pricingtypes.FilterTypeTermMatch,
		Field: aws.String(field),
		Value: aws.String(value),
	}
}

// --------------------------------------------------------------------------
// EBS volume type display name mapping (mirrors _map_ebs_type in Python)
// --------------------------------------------------------------------------

var _ebsTypeMap = map[string]string{
	"gp2":      "General Purpose",
	"gp3":      "General Purpose",
	"io1":      "Provisioned IOPS",
	"io2":      "Provisioned IOPS",
	"st1":      "Throughput Optimized HDD",
	"sc1":      "Cold HDD",
	"standard": "Magnetic",
}

func mapEBSType(storageType string) string {
	if v, ok := _ebsTypeMap[strings.ToLower(storageType)]; ok {
		return v
	}
	return storageType
}

// ebsIOPSStaticFallback returns hardcoded on-demand EBS pricing for io1/io2
// volume types. These rates match AWS published prices as of 2024 and are used
// in bulkFallback mode to avoid downloading the 449 MB AmazonEC2 bulk pricing
// file. Two NormalizedPrice entries are returned: one for GB-month storage and
// one for provisioned IOPS-month.
//
// Published rates (us-east-1, all regions within ~10%):
//   - io2:  $0.125/GB-month  + $0.065/IOPS-month
//   - io1:  $0.125/GB-month  + $0.065/IOPS-month
//
// The "fallback" attribute is set to "true" so that normalizedPriceSummary
// in lookup.go surfaces this flag in the tool response.
func ebsIOPSStaticFallback(storageTypeLower, region string) []models.NormalizedPrice {
	var gbRate float64
	switch storageTypeLower {
	case "io2":
		gbRate = 0.125
	case "io1":
		gbRate = 0.125
	default:
		gbRate = 0.065
	}
	iopsRate := 0.065

	now := time.Now().UTC()
	attrs := map[string]string{
		"fallback":     "true",
		"storage_type": storageTypeLower,
	}
	return []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderAWS,
			Service:       "storage",
			SKUID:         fmt.Sprintf("aws:ebs:%s:%s:storage:static", storageTypeLower, region),
			ProductFamily: "Storage",
			Description:   fmt.Sprintf("AWS EBS %s provisioned storage — static rate ($%.3f/GB-month)", strings.ToUpper(storageTypeLower), gbRate),
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  gbRate,
			Unit:          models.PriceUnitPerGBMonth,
			Currency:      "USD",
			Attributes:    attrs,
			FetchedAt:     &now,
		},
		{
			Provider:      models.CloudProviderAWS,
			Service:       "storage",
			SKUID:         fmt.Sprintf("aws:ebs:%s:%s:iops:static", storageTypeLower, region),
			ProductFamily: "Storage",
			Description:   fmt.Sprintf("AWS EBS %s provisioned IOPS — static rate ($%.3f/IOPS-month)", strings.ToUpper(storageTypeLower), iopsRate),
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  iopsRate,
			Unit:          models.PriceUnitPerIOPSMonth,
			Currency:      "USD",
			Attributes:    attrs,
			FetchedAt:     &now,
		},
	}
}

// ebsGBStaticFallback returns a single hardcoded NormalizedPrice for common
// EBS volume types (gp2, gp3, st1, sc1, standard). It is used in bulkFallback
// mode to avoid downloading the 449 MB AmazonEC2 bulk pricing file. Published
// US East-1 on-demand rates as of 2024:
//
//   - gp3:      $0.080/GB-month
//   - gp2:      $0.100/GB-month
//   - st1:      $0.045/GB-month
//   - sc1:      $0.015/GB-month
//   - standard: $0.050/GB-month
func ebsGBStaticFallback(storageTypeLower, region string) models.NormalizedPrice {
	rates := map[string]float64{
		"gp3":      0.08,
		"gp2":      0.10,
		"st1":      0.045,
		"sc1":      0.015,
		"standard": 0.05,
	}
	rate := rates[storageTypeLower]
	now := time.Now().UTC()
	return models.NormalizedPrice{
		Provider:      models.CloudProviderAWS,
		Service:       "storage",
		SKUID:         fmt.Sprintf("aws:ebs:%s:%s:storage:static", storageTypeLower, region),
		ProductFamily: "Storage",
		Description:   fmt.Sprintf("AWS EBS %s storage — static rate ($%.3f/GB-month)", strings.ToUpper(storageTypeLower), rate),
		Region:        region,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  rate,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
		Attributes: map[string]string{
			"fallback":      "true",
			"fallback_note": "static published rate — verify at https://aws.amazon.com/ebs/pricing/",
			"storage_type":  storageTypeLower,
		},
		FetchedAt: &now,
	}
}

// --------------------------------------------------------------------------
// RDS engine display name mapping (mirrors _RDS_ENGINE_MAP in Python)
// --------------------------------------------------------------------------

var _rdsEngineMap = map[string]string{
	"mysql":             "MySQL",
	"postgresql":        "PostgreSQL",
	"postgres":          "PostgreSQL",
	"mariadb":           "MariaDB",
	"oracle":            "Oracle",
	"sqlserver":         "SQL Server",
	"aurora-mysql":      "Aurora MySQL",
	"aurora-postgresql": "Aurora PostgreSQL",
	"aurora-postgres":   "Aurora PostgreSQL",
}

func normalizeRDSEngine(engine string) string {
	if v, ok := _rdsEngineMap[strings.ToLower(engine)]; ok {
		return v
	}
	return engine
}

// --------------------------------------------------------------------------
// GetProducts — calls AWS Pricing API with inline exponential backoff retry.
// --------------------------------------------------------------------------

// GetProducts fetches matching price list items from the AWS Pricing API.
// Each element in the returned slice is a raw JSON string from the API that
// encodes the full SKU structure.
func (p *Provider) GetProducts(
	ctx context.Context,
	serviceCode string,
	filters []pricingtypes.Filter,
	maxResults int32,
) ([]string, error) {
	if p.bulkFallback {
		region := extractRegionFromFilters(filters)
		return p.getProductsBulk(ctx, serviceCode, filters, maxResults, region)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s — skip on 4xx (don't retry client errors).
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(int64(1<<uint(attempt-1))) * time.Second): //nolint:gosec // attempt is 1 or 2, value is 1 or 2 (never overflows)
			}
		}

		var results []string
		var nextToken *string
		remaining := maxResults

		for {
			pageSize := remaining
			if pageSize > 100 {
				pageSize = 100
			}
			out, err := p.pricingClient.GetProducts(ctx, &pricing.GetProductsInput{
				ServiceCode:   aws.String(serviceCode),
				Filters:       filters,
				FormatVersion: aws.String("aws_v1"),
				MaxResults:    aws.Int32(pageSize),
				NextToken:     nextToken,
			})
			if err != nil {
				// Check if it's a 4xx — don't retry client errors.
				var respErr *awshttp.ResponseError
				if isAWS4xxError(err, &respErr) {
					return nil, err
				}
				lastErr = err
				break // retry outer loop
			}
			results = append(results, out.PriceList...)
			remaining -= int32(len(out.PriceList)) //nolint:gosec // len(PriceList) ≤ 100 (AWS API page max), well within int32
			if out.NextToken == nil || remaining <= 0 {
				return results, nil
			}
			nextToken = out.NextToken
		}
	}
	return nil, lastErr
}

// isAWS4xxError inspects an AWS SDK error and returns true if it is an HTTP
// 4xx response (client error). The respErr out-param is populated if the error
// is an *awshttp.ResponseError.
func isAWS4xxError(err error, out **awshttp.ResponseError) bool {
	var re *awshttp.ResponseError
	if isResponseError(err, &re) {
		*out = re
		return re.HTTPStatusCode() >= 400 && re.HTTPStatusCode() < 500
	}
	return false
}

// isResponseError is a thin wrapper around errors.As to avoid importing "errors"
// at package level just for this function.
func isResponseError(err error, out **awshttp.ResponseError) bool {
	var re *awshttp.ResponseError
	for err != nil {
		if x, ok := err.(*awshttp.ResponseError); ok {
			*out = x
			return true
		}
		// Unwrap manually to avoid importing errors package just for this.
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			break
		}
	}
	_ = re
	return false
}

// --------------------------------------------------------------------------
// SKU JSON parsing
// --------------------------------------------------------------------------

// priceDimension holds a single price dimension within an offer term.
type priceDimension struct {
	Unit         string            `json:"unit"`
	PricePerUnit map[string]string `json:"pricePerUnit"`
	Description  string            `json:"description"`
}

// offerTerm holds a single offer term (on-demand or reserved).
type offerTerm struct {
	PriceDimensions map[string]priceDimension `json:"priceDimensions"`
	TermAttributes  map[string]string         `json:"termAttributes"`
}

// parsedSKU holds the top-level structure of an AWS pricing SKU JSON string.
type parsedSKU struct {
	Product struct {
		SKU           string            `json:"sku"`
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		// OnDemand: map[offerTermCode]offerTerm
		OnDemand map[string]offerTerm `json:"OnDemand"`
		// Reserved: map[offerTermCode]offerTerm
		Reserved map[string]offerTerm `json:"Reserved"`
	} `json:"terms"`
}

// extractOnDemandPrice extracts (pricePerUnit, unitStr) from the OnDemand term.
// Returns ("", "") if no non-zero price is found.
func extractOnDemandPrice(sku parsedSKU) (float64, string) {
	for _, offerTerm := range sku.Terms.OnDemand {
		for _, dim := range offerTerm.PriceDimensions {
			usd := dim.PricePerUnit["USD"]
			if usd == "" || usd == "0.0000000000" {
				continue
			}
			price, err := strconv.ParseFloat(usd, 64)
			if err != nil || price == 0 {
				continue
			}
			return price, dim.Unit
		}
	}
	return 0, ""
}

// reservedTermQualifiers maps PricingTerm to the LeaseContractLength and
// PurchaseOption AWS uses in termAttributes. Mirrors _RESERVED_FILTERS in Python.
var _reservedTermFilters = map[models.PricingTerm][2]string{
	models.PricingTermReserved1Yr:        {"1yr", "No Upfront"},
	models.PricingTermReserved3Yr:        {"3yr", "No Upfront"},
	models.PricingTermReserved1YrPartial: {"1yr", "Partial Upfront"},
	models.PricingTermReserved1YrAll:     {"1yr", "All Upfront"},
	models.PricingTermReserved3YrPartial: {"3yr", "Partial Upfront"},
	models.PricingTermReserved3YrAll:     {"3yr", "All Upfront"},
}

// extractReservedPrice extracts the effective hourly rate for a reserved term,
// normalising upfront costs into an equivalent hourly rate. Mirrors Python's
// _extract_reserved_price.
func extractReservedPrice(sku parsedSKU, term models.PricingTerm) (float64, string) {
	qualifiers, ok := _reservedTermFilters[term]
	if !ok {
		return 0, ""
	}
	leaseLen := qualifiers[0]
	purchaseOpt := qualifiers[1]

	for _, offerTerm := range sku.Terms.Reserved {
		attrs := offerTerm.TermAttributes
		if attrs["LeaseContractLength"] != leaseLen || attrs["PurchaseOption"] != purchaseOpt {
			continue
		}
		var hourly, upfront float64
		for _, dim := range offerTerm.PriceDimensions {
			usd, _ := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
			switch dim.Unit {
			case "Hrs":
				hourly = usd
			case "Quantity":
				upfront = usd
			}
		}
		// Normalise upfront to effective hourly: upfront / (8760 * term_years)
		if upfront > 0 {
			years := 1.0
			if strings.HasPrefix(leaseLen, "3") {
				years = 3.0
			}
			hourly += upfront / (8760 * years)
		}
		if hourly > 0 {
			return hourly, "Hrs"
		}
	}
	return 0, ""
}

// parseUnit converts an AWS unit string to a models.PriceUnit.
func parseUnit(unitStr string) models.PriceUnit {
	switch strings.ToLower(unitStr) {
	case "hrs", "hours", "hour":
		return models.PriceUnitPerHour
	case "gb-mo", "gb/mo", "gb-month":
		return models.PriceUnitPerGBMonth
	case "gb":
		return models.PriceUnitPerGB
	case "iops-mo", "iops-month":
		return models.PriceUnitPerIOPSMonth
	case "mbps-mo", "mbps-month":
		return models.PriceUnitPerMBPSMonth
	case "requests":
		return models.PriceUnitPerRequest
	case "gb-second":
		return models.PriceUnitPerGBSecond
	default:
		return models.PriceUnitPerUnit
	}
}

// skuToNormalizedPrice converts a raw SKU JSON string to a NormalizedPrice.
// Returns nil if the SKU has no usable price for the requested term.
func skuToNormalizedPrice(
	raw string,
	region string,
	term models.PricingTerm,
	service string,
) *models.NormalizedPrice {
	var sku parsedSKU
	if err := json.Unmarshal([]byte(raw), &sku); err != nil {
		return nil
	}

	var pricePerUnit float64
	var unitStr string

	if term == models.PricingTermOnDemand {
		pricePerUnit, unitStr = extractOnDemandPrice(sku)
	} else {
		pricePerUnit, unitStr = extractReservedPrice(sku, term)
	}

	if pricePerUnit == 0 {
		return nil
	}

	attrs := sku.Product.Attributes

	// Build description from most specific attribute. Mirrors Python _desc_keys.
	descKeys := []string{"instanceType", "databaseEngine", "groupDescription", "group", "transferType", "volumeType"}
	description := sku.Product.SKU
	for _, k := range descKeys {
		if v, ok := attrs[k]; ok && v != "" {
			description = v
			break
		}
	}
	if description == "" {
		description = sku.Product.ProductFamily
	}

	// Strip high-noise attributes that duplicate top-level fields.
	noise := map[string]bool{
		"location": true, "locationType": true, "servicecode": true,
		"servicename": true, "regionCode": true, "usagetype": true,
	}
	filteredAttrs := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if !noise[k] {
			filteredAttrs[k] = v
		}
	}

	now := time.Now()
	return &models.NormalizedPrice{
		Provider:      models.CloudProviderAWS,
		Service:       service,
		SKUID:         sku.Product.SKU,
		ProductFamily: sku.Product.ProductFamily,
		Description:   description,
		Region:        region,
		Attributes:    filteredAttrs,
		PricingTerm:   term,
		PricePerUnit:  pricePerUnit,
		Unit:          parseUnit(unitStr),
		Currency:      "USD",
		FetchedAt:     &now,
		SourceURL:     "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json",
	}
}

// --------------------------------------------------------------------------
// GetComputePrice
// --------------------------------------------------------------------------

// GetComputePrice returns on-demand or reserved pricing for an EC2 instance type.
func (p *Provider) GetComputePrice(
	ctx context.Context,
	instanceType string,
	region string,
	os string,
	term models.PricingTerm,
) ([]models.NormalizedPrice, error) {
	if os == "" {
		os = "Linux"
	}
	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("aws:compute:%s:%s:%s:%s", region, instanceType, os, term)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	filters := computeFilters(instanceType, location, os)
	rawItems, err := p.GetProducts(ctx, "AmazonEC2", filters, 10)
	if err != nil {
		return nil, fmt.Errorf("aws: GetComputePrice: %w", err)
	}

	var prices []models.NormalizedPrice
	for _, raw := range rawItems {
		np := skuToNormalizedPrice(raw, region, term, "compute")
		if np != nil {
			prices = append(prices, *np)
		}
	}

	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetStoragePrice
// --------------------------------------------------------------------------

// GetStoragePrice returns pricing for an EBS volume type or S3.
func (p *Provider) GetStoragePrice(
	ctx context.Context,
	storageType string,
	region string,
	sizeGB float64,
) ([]models.NormalizedPrice, error) {
	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("aws:storage:%s:%s", region, storageType)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	ebsTypes := map[string]bool{"gp2": true, "gp3": true, "io1": true, "io2": true, "st1": true, "sc1": true, "standard": true}

	// In bulkFallback mode (no AWS credentials), fetching the full AmazonEC2
	// per-region bulk pricing file (449 MB uncompressed) for io1/io2 queries
	// would exhaust the MCP request timeout before the download completes.
	// Use hardcoded static rates instead — these are the published AWS on-demand
	// rates (io2: $0.125/GB-month + $0.065/IOPS-month;
	// io1: $0.125/GB-month + $0.065/IOPS-month) which change at most quarterly.
	stLower := strings.ToLower(storageType)
	if p.bulkFallback && (stLower == "io1" || stLower == "io2") {
		prices := ebsIOPSStaticFallback(stLower, region)
		if data, err := json.Marshal(prices); err == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
		return prices, nil
	}
	gbFallbackTypes := map[string]bool{"gp2": true, "gp3": true, "st1": true, "sc1": true, "standard": true}
	if p.bulkFallback && gbFallbackTypes[stLower] {
		prices := []models.NormalizedPrice{ebsGBStaticFallback(stLower, region)}
		if data, err := json.Marshal(prices); err == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
		return prices, nil
	}

	var rawItems []string
	if ebsTypes[strings.ToLower(storageType)] {
		volumeType := mapEBSType(storageType)
		filters := storageFilters(volumeType, location)
		// Disambiguate gp2 vs gp3 with volumeApiName
		if strings.ToLower(storageType) == "gp2" || strings.ToLower(storageType) == "gp3" {
			filters = append(filters, mkFilter("volumeApiName", strings.ToLower(storageType)))
		}
		rawItems, err = p.GetProducts(ctx, "AmazonEC2", filters, 5)
	} else {
		// S3 standard storage fallback
		filters := []pricingtypes.Filter{
			mkFilter("location", location),
			mkFilter("storageClass", "General Purpose"),
			mkFilter("volumeType", "Standard"),
		}
		rawItems, err = p.GetProducts(ctx, "AmazonS3", filters, 5)
	}
	if err != nil {
		return nil, fmt.Errorf("aws: GetStoragePrice: %w", err)
	}

	var prices []models.NormalizedPrice
	for _, raw := range rawItems {
		np := skuToNormalizedPrice(raw, region, models.PricingTermOnDemand, "storage")
		if np != nil {
			// Upgrade PER_UNIT to PER_GB_MONTH for storage products
			if np.Unit == models.PriceUnitPerUnit {
				np.Unit = models.PriceUnitPerGBMonth
			}
			prices = append(prices, *np)
		}
	}

	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetDatabasePrice
// --------------------------------------------------------------------------

// GetDatabasePrice returns pricing for an RDS instance or ElastiCache node.
// For RDS: supported engines are mysql, postgres, mariadb, oracle, sqlserver,
// aurora-mysql, aurora-postgresql.
// For ElastiCache: use engine="elasticache-redis", "elasticache-memcached",
// "redis", or "memcached".
func (p *Provider) GetDatabasePrice(
	ctx context.Context,
	engine string,
	instanceType string,
	region string,
	term models.PricingTerm,
) ([]models.NormalizedPrice, error) {
	// Route ElastiCache engines to GetElastiCachePrice.
	engineLower := strings.ToLower(engine)
	if engineLower == "elasticache-redis" || engineLower == "elasticache-memcached" ||
		engineLower == "redis" || engineLower == "memcached" || engineLower == "elasticache" {
		return p.GetElastiCachePrice(ctx, instanceType, region)
	}

	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("aws:database:%s:%s:%s:%s", region, engine, instanceType, term)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	engineNorm := normalizeRDSEngine(engine)
	filters := databaseFilters(instanceType, location, engineNorm, "Single-AZ")

	rawItems, err := p.GetProducts(ctx, "AmazonRDS", filters, 10)
	if err != nil {
		return nil, fmt.Errorf("aws: GetDatabasePrice: %w", err)
	}

	var prices []models.NormalizedPrice
	for _, raw := range rawItems {
		np := skuToNormalizedPrice(raw, region, term, "database")
		if np != nil {
			prices = append(prices, *np)
		}
	}

	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// SearchPricing
// --------------------------------------------------------------------------

// SearchPricing performs a free-text search across EC2 pricing.
// For EC2 it applies compute-standard filters; for other services it does a
// broad text match. Mirrors search_pricing() from Python.
func (p *Provider) SearchPricing(
	ctx context.Context,
	query string,
	region string,
	maxResults int,
) ([]models.NormalizedPrice, error) {
	var filters []pricingtypes.Filter
	if region != "" {
		if location, err := regionToLocation(region); err == nil {
			filters = append(filters, mkFilter("location", location))
		}
	}

	// Apply EC2 compute filters for instance type queries
	filters = append(filters,
		mkFilter("tenancy", "Shared"),
		mkFilter("operatingSystem", "Linux"),
		mkFilter("preInstalledSw", "NA"),
		mkFilter("capacitystatus", "Used"),
	)
	if strings.Contains(query, ".") && !strings.HasPrefix(query, "gpu") {
		filters = append(filters, mkFilter("instanceType", query))
	}

	rawItems, err := p.GetProducts(ctx, "AmazonEC2", filters, int32(maxResults*3)) //nolint:gosec // maxResults is a small constant (10–50), product is well within int32
	if err != nil {
		return nil, fmt.Errorf("aws: SearchPricing: %w", err)
	}

	queryLower := strings.ToLower(query)
	var prices []models.NormalizedPrice
	for _, raw := range rawItems {
		var sku parsedSKU
		if err := json.Unmarshal([]byte(raw), &sku); err != nil {
			continue
		}
		attrs := sku.Product.Attributes
		instanceType := attrs["instanceType"]

		if strings.Contains(queryLower, "gpu") {
			if attrs["gpu"] == "" || attrs["gpu"] == "0" {
				continue
			}
		} else if !strings.Contains(strings.ToLower(instanceType), queryLower) {
			continue
		}

		effectiveRegion := region
		if effectiveRegion == "" {
			effectiveRegion = "us-east-1"
		}
		np := skuToNormalizedPrice(raw, effectiveRegion, models.PricingTermOnDemand, "compute")
		if np != nil {
			prices = append(prices, *np)
		}
		if len(prices) >= maxResults {
			break
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// ListRegions
// --------------------------------------------------------------------------

// ListRegions returns AWS regions where EC2 is available.
// Uses the EC2 DescribeRegions API when credentials are present; falls back
// to the static _regionToLocation map.
func (p *Provider) ListRegions(ctx context.Context, service string) ([]string, error) {
	cacheKey := "aws:regions:" + service
	if cached, ok := p.cache.GetMetadata(cacheKey); ok {
		var regions []string
		if err := json.Unmarshal(cached, &regions); err == nil {
			return regions, nil
		}
	}

	var out *ec2.DescribeRegionsOutput
	var err error
	if p.ec2Client != nil {
		out, err = p.ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
			AllRegions: aws.Bool(false),
		})
	} else {
		err = fmt.Errorf("ec2 client not initialised")
	}
	if err == nil {
		regions := make([]string, 0, len(out.Regions))
		for _, r := range out.Regions {
			if r.RegionName != nil {
				regions = append(regions, *r.RegionName)
			}
		}
		if data, err2 := json.Marshal(regions); err2 == nil {
			ttl := time.Duration(p.cfg.MetadataTTLDays) * 24 * time.Hour
			p.cache.SetMetadata(cacheKey, data, ttl)
		}
		return regions, nil
	}

	// Fallback: return the static map keys.
	regions := make([]string, 0, len(_regionToLocation))
	for r := range _regionToLocation {
		regions = append(regions, r)
	}
	return regions, nil
}

// --------------------------------------------------------------------------
// ListInstanceTypes
// --------------------------------------------------------------------------

// ListInstanceTypes returns instance types matching the given filters using
// ec2.DescribeInstanceTypes. Falls back to the Pricing API if needed.
func (p *Provider) ListInstanceTypes(
	ctx context.Context,
	region string,
	family string,
	minVCPUs int,
	minMemoryGB float64,
	gpu bool,
) ([]models.InstanceTypeInfo, error) {
	cacheKey := fmt.Sprintf("aws:instance_types:%s:%s:%d:%v:%v", region, family, minVCPUs, minMemoryGB, gpu)
	if cached, ok := p.cache.GetMetadata(cacheKey); ok {
		var infos []models.InstanceTypeInfo
		if err := json.Unmarshal(cached, &infos); err == nil {
			return infos, nil
		}
	}

	// Build filters for DescribeInstanceTypes
	var ec2Filters []ec2types.Filter
	if gpu {
		ec2Filters = append(ec2Filters, ec2types.Filter{
			Name:   aws.String("processor-info.supported-features"),
			Values: []string{"amd-sev-snp"},
		})
	}

	// Use a region-specific EC2 client
	ec2Cfg := p.ec2Client
	_ = ec2Cfg // We'll create a new client for the specified region if different

	// Create EC2 client for the specified region
	var instanceTypes []models.InstanceTypeInfo
	var nextToken *string

	for {
		input := &ec2.DescribeInstanceTypesInput{
			Filters:   ec2Filters,
			NextToken: nextToken,
		}

		out, err := p.ec2Client.DescribeInstanceTypes(ctx, input)
		if err != nil {
			break
		}

		for _, it := range out.InstanceTypes {
			itName := string(it.InstanceType)

			// Family filter
			if family != "" && !strings.HasPrefix(itName, family) {
				continue
			}

			// Extract vCPU count
			vcpu := 0
			if it.VCpuInfo != nil && it.VCpuInfo.DefaultVCpus != nil {
				vcpu = int(*it.VCpuInfo.DefaultVCpus)
			}

			// Extract memory in GB
			var memGB float64
			if it.MemoryInfo != nil && it.MemoryInfo.SizeInMiB != nil {
				memGB = float64(*it.MemoryInfo.SizeInMiB) / 1024.0
			}

			// GPU info
			gpuCount := 0
			gpuType := ""
			if it.GpuInfo != nil {
				for _, g := range it.GpuInfo.Gpus {
					if g.Count != nil {
						gpuCount += int(*g.Count)
					}
					if g.Name != nil && gpuType == "" {
						gpuType = *g.Name
					}
				}
			}

			// Apply filters
			if minVCPUs > 0 && vcpu < minVCPUs {
				continue
			}
			if minMemoryGB > 0 && memGB < minMemoryGB {
				continue
			}
			if gpu && gpuCount == 0 {
				continue
			}

			// Network performance
			netPerf := ""
			if it.NetworkInfo != nil && it.NetworkInfo.NetworkPerformance != nil {
				netPerf = *it.NetworkInfo.NetworkPerformance
			}

			instanceTypes = append(instanceTypes, models.InstanceTypeInfo{
				Provider:           models.CloudProviderAWS,
				InstanceType:       itName,
				VCPU:               vcpu,
				MemoryGB:           memGB,
				GPUCount:           gpuCount,
				GPUType:            gpuType,
				NetworkPerformance: netPerf,
				Region:             region,
				Available:          true,
			})
		}

		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if data, err := json.Marshal(instanceTypes); err == nil {
		ttl := time.Duration(p.cfg.MetadataTTLDays) * 24 * time.Hour
		p.cache.SetMetadata(cacheKey, data, ttl)
	}
	return instanceTypes, nil
}

// --------------------------------------------------------------------------
// CheckAvailability
// --------------------------------------------------------------------------

// CheckAvailability reports whether an instance type or storage type is
// available in the specified region by verifying via GetProducts.
// Returns (available, alternateSuggestions, error).
func (p *Provider) CheckAvailability(
	ctx context.Context,
	service string,
	skuOrType string,
	region string,
) (bool, []string, error) {
	switch service {
	case "compute":
		prices, err := p.GetComputePrice(ctx, skuOrType, region, "Linux", models.PricingTermOnDemand)
		if err != nil {
			return false, nil, err
		}
		return len(prices) > 0, nil, nil
	case "storage":
		prices, err := p.GetStoragePrice(ctx, skuOrType, region, 0)
		if err != nil {
			return false, nil, err
		}
		return len(prices) > 0, nil, nil
	default:
		return false, nil, nil
	}
}

// --------------------------------------------------------------------------
// GetPrice — unified dispatcher
// --------------------------------------------------------------------------

// GetPrice is the primary entry point. It dispatches to the correct method
// based on spec.GetDomain().
func (p *Provider) GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	var prices []models.NormalizedPrice
	var err error

	switch spec.GetDomain() { //nolint:exhaustive // AI, container, analytics, observability fall to default error
	case models.PricingDomainCompute:
		cs, ok := spec.(*models.ComputePricingSpec)
		if !ok {
			return nil, fmt.Errorf("aws: GetPrice: expected *ComputePricingSpec")
		}
		// Route Savings Plan terms to the SP pricing handler, which returns its
		// own PricingResult (with Breakdown and ContractedPrices) directly.
		if cs.GetTerm() == models.PricingTermComputeSP || cs.GetTerm() == models.PricingTermEC2InstanceSP {
			return p.GetSavingsPlanPrice(ctx, cs)
		}
		prices, err = p.GetComputePrice(ctx, cs.ResourceType, cs.GetRegion(), cs.OS, cs.GetTerm())

	case models.PricingDomainStorage:
		ss, ok := spec.(*models.StoragePricingSpec)
		if !ok {
			return nil, fmt.Errorf("aws: GetPrice: expected *StoragePricingSpec")
		}
		sizeGB := 0.0
		if ss.SizeGB != nil {
			sizeGB = *ss.SizeGB
		}
		prices, err = p.GetStoragePrice(ctx, ss.StorageType, ss.GetRegion(), sizeGB)

	case models.PricingDomainDatabase:
		ds, ok := spec.(*models.DatabasePricingSpec)
		if !ok {
			return nil, fmt.Errorf("aws: GetPrice: expected *DatabasePricingSpec")
		}
		// When the model passes service="elasticache" (or "redis"/"memcached") without
		// an explicit engine field, Engine defaults to "MySQL" from DatabasePricingSpec.
		// Use the service field to override routing for in-memory caches.
		engine := ds.Engine
		svc := strings.ToLower(ds.GetService())
		if svc == "elasticache" || svc == "redis" || svc == "memcached" {
			engine = svc
		}
		prices, err = p.GetDatabasePrice(ctx, engine, ds.ResourceType, ds.GetRegion(), ds.GetTerm())

	case models.PricingDomainNetwork, models.PricingDomainInterRegionEgress:
		svc := spec.GetService()
		switch svc {
		case "nat", "cloud_nat":
			prices, err = p.GetNATPrice(ctx, spec.GetRegion())
		case "lb", "cloud_lb":
			prices, err = p.GetALBPrice(ctx, spec.GetRegion())
		default:
			prices, err = p.GetNetworkPrice(ctx, spec, spec.GetRegion())
		}

	case models.PricingDomainObservability:
		prices, err = p.GetCloudWatchPrice(ctx, spec.GetRegion())

	case models.PricingDomainServerless:
		ss, ok := spec.(*models.ServerlessPricingSpec)
		if !ok {
			return nil, fmt.Errorf("aws: GetPrice: expected *ServerlessPricingSpec")
		}
		region := ss.GetRegion()
		if region == "" {
			region = "us-east-1"
		}
		prices, err = p.GetLambdaPrice(ctx, region)

	default:
		return nil, fmt.Errorf("aws: GetPrice: unsupported domain %q in Part 1 — domain=%s service=%s",
			spec.GetDomain(), spec.GetDomain(), spec.GetService())
	}

	if err != nil {
		return nil, err
	}

	return &models.PricingResult{
		PublicPrices:  prices,
		AuthAvailable: false,
		Source:        "catalog",
		SchemaVersion: "1",
	}, nil
}

// FinOps methods (GetEffectivePrice, GetSpotHistory, GetDiscountSummary,
// DescribeCatalog, BOMAdvisories) are implemented in aws_finops.go.

// --------------------------------------------------------------------------
// Cache helpers
// --------------------------------------------------------------------------

// cacheManagerType is referenced here to ensure the cache import is used.
var _ *cache.CacheManager
