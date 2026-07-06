// gcp_kms.go — GCP Cloud KMS (Key Management Service) pricing (domain=security, service=kms).
//
// Cloud KMS bills along two independent, region-invariant dimensions:
//   - active key-version-months (a monthly charge per active key version),
//     bifurcated by protection level (software / HSM / external) and, for
//     HSM only, by algorithm; and
//   - cryptographic operation counts (encrypt/decrypt/sign/verify/etc.),
//     bifurcated the same way, plus a separate "Generate Random Bytes" op.
//
// All rates below were verified live against the GCP Cloud Billing Catalog
// API (service ID EE2F-D110-890C, "Cloud Key Management Service (KMS)") and
// cross-checked against https://cloud.google.com/kms/pricing (see issue #77).
// Every one of the 31 in-scope SKUs has geoTaxonomy.type == "GLOBAL" — price
// does not vary by region, so this file never queries or matches on region;
// every returned NormalizedPrice is tagged Region="global" and
// Attributes["scope"]="global", mirroring the network/external_ip (#76)
// precedent in gcp_networking.go.
//
// Explicitly out of scope (not priced by this file): the two Single-Tenant
// Cloud HSM SKUs (4A51-C764-8B93 "Active Single Tenant HSM key versions
// (above 15000)" and EE6E-FF5D-19F0 "Active single tenant HSM instances",
// $3500.00/mo flat) — these belong to a dedicated-instance product with a
// fundamentally different flat-instance-fee + overage pricing model, not the
// per-version/per-operation model this issue describes.
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// kmsServiceID is the GCP Cloud Billing Catalog service ID for
// "Cloud Key Management Service (KMS)" — verified live 2026-07-06. Not to be
// confused with the decoy service "Cloud KMS KACLS" (6594-98DC-0EB3), a
// distinct client-side-encryption access-control product.
const kmsServiceID = "EE2F-D110-890C"

// kmsSourceURL is the canonical public pricing page used for cross-checking
// and as the SourceURL stamped on returned prices.
const kmsSourceURL = "https://cloud.google.com/kms/pricing"

// kmsAutokeyFreeKeyVersions and kmsAutokeyFreeOperations are the Autokey
// monthly free-usage allowances (verified live: tiered SKUs 77F8-D8AF-3CCE
// and 88D6-F2EE-C781 have a $0.00 tier at startUsageAmount=0, then the paid
// rate starting at 100 key versions / 10,000 operations respectively).
//
// Known limitation: these thresholds (and kmsHSMTierThreshold below) are
// hardcoded Go constants rather than being derived from the live SKU
// tieredRates.startUsageAmount values fetchKMSRates already parses. Deriving
// them live would require plumbing thresholds through kmsRates/fetchKMSRates
// as well as rates, which is a larger restructuring than this pass covers;
// GCP changing a tier boundary without also changing the corresponding rate
// is considered unlikely. Revisit if that assumption ever breaks.
const (
	kmsAutokeyFreeKeyVersions = 100
	kmsAutokeyFreeOperations  = 10000
	// kmsHSMTierThreshold is the key-version count above which the volume
	// discount tier applies for HSM asymmetric key versions (verified live:
	// tieredRates startUsageAmount 0 then 2000 on SKUs 1017-1BAF-7159,
	// 83C4-80AE-F5DC, 86EC-5B76-A9BB, 93F6-CB12-A862, A023-0699-5CEC, and
	// 47AB-10E9-7E33).
	kmsHSMTierThreshold = 2000
)

// kmsRates holds the derived, region-invariant Cloud KMS rates, cached under
// kmsRatesCacheKey so repeated calls for different (key_type, algorithm,
// unit) combinations don't re-fetch/re-scan the full KMS SKU catalog.
type kmsRates struct {
	// Key-version-month rates.
	SoftwareKeyVersion    float64 `json:"software_key_version"`     // E09C/7907/1913 — $0.06/mo flat, all algorithms
	HSMKeyVersionFlat     float64 `json:"hsm_key_version_flat"`     // 46B1/1686/BE8A — $1.00/mo flat (symmetric/rsa2048/mac)
	HSMKeyVersionTier1    float64 `json:"hsm_key_version_tier1"`    // 1017/83C4/86EC/93F6/A023/47AB — $2.50/mo (0-2000)
	HSMKeyVersionTier2    float64 `json:"hsm_key_version_tier2"`    // same SKUs — $1.00/mo (2000+)
	ExternalKeyVersion    float64 `json:"external_key_version"`     // D57A/E0AA — $3.00/mo flat
	AutokeyKeyVersionPaid float64 `json:"autokey_key_version_paid"` // 77F8 — $1.00/mo paid rate (100 free/mo)

	// Cryptographic-operation rates (per single operation).
	LowOperation         float64 `json:"low_operation"`          // software/external/hsm-symmetric-rsa2048-mac — $0.000003/op
	HSMOperationHigh     float64 `json:"hsm_operation_high"`     // hsm-ec/rsa3072/rsa4096/pkcs1v15 — $0.000015/op
	AutokeyOperationPaid float64 `json:"autokey_operation_paid"` // 88D6 — $0.000003/op paid rate (10,000 free/mo)
	RandomBytesOperation float64 `json:"random_bytes_operation"` // B9A7 — $0.000015/call
}

// kmsFallbackRates holds the published, live-verified rates (issue #77) used
// when the live SKU catalog is unavailable or a description match fails.
var kmsFallbackRates = kmsRates{
	SoftwareKeyVersion:    0.06,
	HSMKeyVersionFlat:     1.00,
	HSMKeyVersionTier1:    2.50,
	HSMKeyVersionTier2:    1.00,
	ExternalKeyVersion:    3.00,
	AutokeyKeyVersionPaid: 1.00,
	LowOperation:          0.000003,
	HSMOperationHigh:      0.000015,
	AutokeyOperationPaid:  0.000003,
	RandomBytesOperation:  0.000015,
}

// kmsRatesCacheKey caches the derived Cloud KMS rates.
const kmsRatesCacheKey = "gcp:kms:rates"

// fetchKMSRates returns the live, derived Cloud KMS rates, caching the
// result. A zero field means no matching SKU was found for that rate bucket
// and the caller should fall back to kmsFallbackRates.
//
// Matching is case-insensitive substring matching against SKU descriptions
// (never exact-string-equality — a prior code-review finding from #76, since
// GCP catalog wording can drift), reusing the existing skuPrice (first tier,
// startUsageAmount==0) / skuPaidPrice (first tier with startUsageAmount>0)
// tier-selection helpers from gcp.go / gcp_ai.go rather than a new
// duplicate tier-parser.
//
// "for autokey" descriptions are checked first because they are a superset
// substring match of their non-Autokey counterparts (e.g. "Active HSM
// symmetric key versions for Autokey" contains "active hsm symmetric key
// versions" as a literal prefix) — checking generic patterns first would
// mis-route Autokey SKUs into the flat/tiered non-Autokey buckets.
func (p *Provider) fetchKMSRates(ctx context.Context) kmsRates {
	if raw, ok := p.cache.GetMetadata(kmsRatesCacheKey); ok {
		var r kmsRates
		if err := json.Unmarshal(raw, &r); err == nil {
			return r
		}
	}

	var rates kmsRates
	skus, err := p.fetchSKUs(ctx, kmsServiceID)
	if err != nil {
		slog.Warn("gcp kms: fetch SKUs failed", "err", err)
		return rates
	}

	isHighAlgoDesc := func(descLower string) bool {
		return strings.Contains(descLower, "ecdsa") ||
			strings.Contains(descLower, "rsa 3072") ||
			strings.Contains(descLower, "rsa 4096") ||
			strings.Contains(descLower, "pkcs1")
	}

	for _, sku := range skus {
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)

		switch {
		case strings.Contains(descLower, "single tenant"):
			// Out of scope — dedicated-instance flat-fee product, not part of
			// the per-version/per-operation model this issue covers.
			continue

		case strings.Contains(descLower, "generate random bytes"):
			rates.RandomBytesOperation = skuPrice(sku)

		case strings.Contains(descLower, "for autokey") && strings.Contains(descLower, "key version"):
			rates.AutokeyKeyVersionPaid = skuPaidPrice(sku)

		case strings.Contains(descLower, "for autokey"):
			// Autokey cryptographic operations.
			rates.AutokeyOperationPaid = skuPaidPrice(sku)

		case strings.Contains(descLower, "key version"):
			switch {
			case strings.Contains(descLower, "software"):
				rates.SoftwareKeyVersion = skuPrice(sku)
			case strings.Contains(descLower, "external"):
				rates.ExternalKeyVersion = skuPrice(sku)
			case strings.Contains(descLower, "hsm"):
				if isHighAlgoDesc(descLower) {
					rates.HSMKeyVersionTier1 = skuPrice(sku)
					if tier2 := skuPaidPrice(sku); tier2 > 0 {
						rates.HSMKeyVersionTier2 = tier2
					}
				} else {
					rates.HSMKeyVersionFlat = skuPrice(sku)
				}
			}

		case strings.Contains(descLower, "cryptographic operation"):
			switch {
			case strings.Contains(descLower, "hsm") && isHighAlgoDesc(descLower):
				rates.HSMOperationHigh = skuPrice(sku)
			default:
				// software, external, and hsm-symmetric/rsa2048/mac all share
				// the same low operation rate.
				rates.LowOperation = skuPrice(sku)
			}
		}
	}

	if raw, err := json.Marshal(rates); err == nil {
		p.cache.SetMetadata(kmsRatesCacheKey, raw, p.cfg.MetadataTTL())
	}
	return rates
}

// kmsHighAlgorithms is the set of KMSPricingSpec.Algorithm values that carry
// the tiered/high-rate HSM pricing (as opposed to the flat low rate applied
// to symmetric/mac/rsa2048).
var kmsHighAlgorithms = map[string]bool{
	"asymmetric-ec":       true,
	"asymmetric-rsa3072":  true,
	"asymmetric-rsa4096":  true,
	"asymmetric-pkcs1v15": true,
}

// kmsValidKeyTypes, kmsValidAlgorithms, and kmsValidUnits are the complete
// sets of recognized KMSPricingSpec.KeyType / Algorithm / Unit values.
// priceKMS rejects anything outside these sets with an explicit error rather
// than silently falling through the rate-selection switch into a
// wrong-but-plausible bucket (e.g. an unrecognized HSM algorithm landing on
// the flat $1.00/mo rate instead of the intended tiered rate).
var (
	kmsValidKeyTypes = map[string]bool{
		"software": true,
		"hsm":      true,
		"external": true,
	}
	kmsValidAlgorithms = map[string]bool{
		"symmetric":           true,
		"mac":                 true,
		"asymmetric-rsa2048":  true,
		"asymmetric-rsa3072":  true,
		"asymmetric-rsa4096":  true,
		"asymmetric-ec":       true,
		"asymmetric-pkcs1v15": true,
	}
	kmsValidUnits = map[string]bool{
		"key_version_month": true,
		"crypto_operations": true,
		"random_bytes":      true,
	}
)

// pickRate returns live if it is nonzero, otherwise it returns fallback and
// reports usedFallback=true. Centralizing this in one helper means the
// fallback bookkeeping (breakdown["fallback"]=true) cannot be silently
// omitted when a future rate bucket is added to priceKMS.
func pickRate(live, fallback float64) (rate float64, usedFallback bool) {
	if live == 0 {
		return fallback, true
	}
	return live, false
}

// priceKMS returns Cloud KMS pricing for the given KMSPricingSpec.
func (p *Provider) priceKMS(
	ctx context.Context,
	spec *models.KMSPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	rates := p.fetchKMSRates(ctx)

	keyType := strings.ToLower(spec.KeyType)
	if keyType == "" {
		keyType = "software"
	}
	algorithm := strings.ToLower(spec.Algorithm)
	if algorithm == "" {
		algorithm = "symmetric"
	}
	unit := strings.ToLower(spec.Unit)
	if unit == "" {
		unit = "key_version_month"
	}

	// Reject unrecognized enum values explicitly rather than letting them
	// fall through the rate-selection switches below into a
	// wrong-but-plausible bucket (e.g. a typo'd algorithm silently pricing
	// at the flat HSM rate instead of the intended tiered rate). This check
	// runs after the empty-string defaulting above, so omitted fields never
	// error.
	if !kmsValidKeyTypes[keyType] {
		return nil, nil, fmt.Errorf("gcp kms: invalid key_type %q: must be 'software', 'hsm', or 'external'", spec.KeyType)
	}
	if !kmsValidAlgorithms[algorithm] {
		return nil, nil, fmt.Errorf("gcp kms: invalid algorithm %q: must be one of 'symmetric', 'mac', 'asymmetric-rsa2048', 'asymmetric-rsa3072', 'asymmetric-rsa4096', 'asymmetric-ec', 'asymmetric-pkcs1v15'", spec.Algorithm)
	}
	if !kmsValidUnits[unit] {
		return nil, nil, fmt.Errorf("gcp kms: invalid unit %q: must be one of 'key_version_month', 'crypto_operations', 'random_bytes'", spec.Unit)
	}

	isHighAlgo := kmsHighAlgorithms[algorithm]
	limitedAvailability := algorithm == "asymmetric-pkcs1v15" // asia-south1/asia-south2 only, verified live

	var rate, tier2Rate float64
	fallback := false
	// autokeyApplied is set only inside the two rate-selection branches that
	// actually choose an Autokey rate (below) — never derived independently
	// from the spec — so the attrs/breakdown annotations and the
	// monthly_cost math can never diverge from which rate was picked. In
	// particular this keeps unit="random_bytes" (which has no Autokey SKU/
	// free tier at all) from being mistakenly treated as Autokey just
	// because key_type=hsm, algorithm=symmetric, autokey=true happen to be
	// set on the spec.
	autokeyApplied := false
	var description string
	var priceUnit models.PriceUnit

	switch unit {
	case "random_bytes":
		priceUnit = models.PriceUnitPerOperation
		description = "Generate Random Bytes Call"
		rate, fallback = pickRate(rates.RandomBytesOperation, kmsFallbackRates.RandomBytesOperation)

	case "crypto_operations":
		priceUnit = models.PriceUnitPerOperation
		switch {
		// Autokey (verified live: 88D6-F2EE-C781) only exists for HSM
		// symmetric keys; hsm+asymmetric+autokey is an invalid combination
		// that falls through to the ordinary algorithm-specific rate below.
		case spec.Autokey && keyType == "hsm" && algorithm == "symmetric":
			autokeyApplied = true
			description = "HSM symmetric cryptographic operations for Autokey (paid rate; first 10,000 ops/month free)"
			rate, fallback = pickRate(rates.AutokeyOperationPaid, kmsFallbackRates.AutokeyOperationPaid)
		case keyType == "hsm" && isHighAlgo:
			description = fmt.Sprintf("HSM cryptographic operations with a %s key", algorithm)
			rate, fallback = pickRate(rates.HSMOperationHigh, kmsFallbackRates.HSMOperationHigh)
		default:
			description = fmt.Sprintf("%s cryptographic operations (%s)", keyType, algorithm)
			rate, fallback = pickRate(rates.LowOperation, kmsFallbackRates.LowOperation)
		}

	default: // "key_version_month"
		priceUnit = models.PriceUnitPerKeyVersionMonth
		switch {
		case keyType == "external":
			description = "Active external key versions"
			rate, fallback = pickRate(rates.ExternalKeyVersion, kmsFallbackRates.ExternalKeyVersion)
		// Autokey (verified live: 77F8-D8AF-3CCE) only exists for HSM
		// symmetric keys; hsm+asymmetric+autokey is an invalid combination
		// that falls through to the tiered/flat rate below instead.
		case keyType == "hsm" && algorithm == "symmetric" && spec.Autokey:
			autokeyApplied = true
			description = "Active HSM symmetric key versions for Autokey (paid rate; first 100 versions/month free)"
			rate, fallback = pickRate(rates.AutokeyKeyVersionPaid, kmsFallbackRates.AutokeyKeyVersionPaid)
		case keyType == "hsm" && isHighAlgo:
			description = fmt.Sprintf("Active HSM %s key versions", algorithm)
			// A tiered rate is only fully resolved when BOTH tiers come back
			// nonzero from the live catalog; if either is zero (e.g. a live
			// response populates tier1 but omits tier2), both must fall back
			// together so tier2 is never silently priced at $0.00.
			var fb1, fb2 bool
			rate, fb1 = pickRate(rates.HSMKeyVersionTier1, kmsFallbackRates.HSMKeyVersionTier1)
			tier2Rate, fb2 = pickRate(rates.HSMKeyVersionTier2, kmsFallbackRates.HSMKeyVersionTier2)
			if fb1 || fb2 {
				fallback = true
				rate = kmsFallbackRates.HSMKeyVersionTier1
				tier2Rate = kmsFallbackRates.HSMKeyVersionTier2
			}
		case keyType == "hsm":
			description = fmt.Sprintf("Active HSM %s key versions", algorithm)
			rate, fallback = pickRate(rates.HSMKeyVersionFlat, kmsFallbackRates.HSMKeyVersionFlat)
		default: // software
			description = "Active software key versions"
			rate, fallback = pickRate(rates.SoftwareKeyVersion, kmsFallbackRates.SoftwareKeyVersion)
		}
	}

	attrs := map[string]string{
		"key_type":  keyType,
		"algorithm": algorithm,
		"unit":      unit,
	}
	if autokeyApplied {
		attrs["autokey"] = "true"
	}
	if limitedAvailability {
		attrs["availability"] = "asia-south1, asia-south2 only"
	}

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "kms",
		SKUID:         fmt.Sprintf("gcp:kms:%s:%s:%s", keyType, algorithm, unit),
		ProductFamily: "Cloud KMS",
		Description:   description,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  rate,
		Unit:          priceUnit,
		Currency:      "USD",
		Attributes:    attrs,
	}
	stampGlobalScope(&price)

	breakdown := map[string]any{
		"key_type":  keyType,
		"algorithm": algorithm,
		"unit":      unit,
	}
	if fallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rate; live SKU catalog unavailable or returned no match. Verify current rates at " + kmsSourceURL + "."
	}
	// tier2Rate is only ever populated (nonzero) by the tiered HSM-asymmetric
	// branch above, so it doubles as the "was this priced as tiered" flag
	// without a separate bool to keep in sync.
	if tier2Rate != 0 {
		breakdown["tier1_rate"] = breakdownMoney(rate, "/mo (0-2,000 key versions)")
		breakdown["tier2_rate"] = breakdownMoney(tier2Rate, "/mo (2,000+ key versions)")
		breakdown["tier_threshold_key_versions"] = kmsHSMTierThreshold
	}
	if autokeyApplied {
		if unit == "key_version_month" {
			breakdown["free_tier_key_versions_per_month"] = kmsAutokeyFreeKeyVersions
		} else {
			breakdown["free_tier_operations_per_month"] = kmsAutokeyFreeOperations
		}
		breakdown["autokey_note"] = "Headline rate is the PAID rate that applies after the Autokey free allowance is exceeded; usage within the free allowance is $0.00."
	}
	if limitedAvailability {
		breakdown["availability_note"] = "PKCS1 v1.5 HSM keys are only available in asia-south1 and asia-south2 (verified live serviceRegions); price is identical to other tiered HSM asymmetric algorithms wherever available."
	}

	// monthly_cost must account for the same tiering/free-allowance
	// structure surfaced above — a flat rate*quantity product silently
	// overcharges for tiered HSM asymmetric key versions (rate here is only
	// the tier1 rate) and ignores the Autokey free allowance entirely.
	// All three shapes (flat, tiered volume-discount, and free-then-paid
	// Autokey allowance) are expressed as an []egressTier list and priced
	// through the shared computeTieredCost helper (gcp_networking.go) rather
	// than each hand-rolling its own clamp-and-subtract arithmetic.
	// egressTier.thresholdGB is reused here as a generic tier threshold
	// (key-version count or operation count, not gigabytes) — the field name
	// is a holdover from its original egress-pricing use.
	switch {
	case unit == "key_version_month" && spec.KeyVersions != nil:
		qty := *spec.KeyVersions
		var tiers []egressTier
		switch {
		case tier2Rate != 0:
			tiers = []egressTier{
				{thresholdGB: 0, rate: rate, label: "tier1"},
				{thresholdGB: kmsHSMTierThreshold, rate: tier2Rate, label: "tier2"},
			}
		case autokeyApplied:
			tiers = []egressTier{
				{thresholdGB: 0, rate: 0, label: "free"},
				{thresholdGB: kmsAutokeyFreeKeyVersions, rate: rate, label: "paid"},
			}
		default:
			tiers = []egressTier{{thresholdGB: 0, rate: rate, label: "flat"}}
		}
		cost := computeTieredCost(tiers, qty).TotalCost
		breakdown["monthly_cost"] = breakdownMoney(cost, "/mo")
	case (unit == "crypto_operations" || unit == "random_bytes") && spec.OperationsPerMonth != nil:
		ops := *spec.OperationsPerMonth
		var tiers []egressTier
		if autokeyApplied {
			tiers = []egressTier{
				{thresholdGB: 0, rate: 0, label: "free"},
				{thresholdGB: kmsAutokeyFreeOperations, rate: rate, label: "paid"},
			}
		} else {
			tiers = []egressTier{{thresholdGB: 0, rate: rate, label: "flat"}}
		}
		cost := computeTieredCost(tiers, ops).TotalCost
		breakdown["monthly_cost"] = breakdownMoney(cost, "/mo")
	}

	return annotateFreshWithURL([]models.NormalizedPrice{price}, kmsSourceURL), breakdown, nil
}
