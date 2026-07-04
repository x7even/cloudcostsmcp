package aws

import (
	"context"
	"net/http"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// eksBulkJSON is a minimal bulk JSON fixture mirroring the real AmazonEKS
// offer file structure: a standard per-cluster SKU, an extended-support
// surcharge SKU, and an Outposts per-cluster SKU that must NOT be matched
// (it shares operation=CreateOperation with the standard SKU but has a
// different locationType).
const eksBulkJSON = `{
  "products": {
    "STANDARD1": {
      "sku": "STANDARD1",
      "productFamily": "Compute",
      "attributes": {
        "servicecode": "AmazonEKS",
        "location": "US East (N. Virginia)",
        "locationType": "AWS Region",
        "usagetype": "USE1-AmazonEKS-Hours:perCluster",
        "operation": "CreateOperation"
      }
    },
    "EXTSUPPORT1": {
      "sku": "EXTSUPPORT1",
      "productFamily": "Compute",
      "attributes": {
        "servicecode": "AmazonEKS",
        "location": "US East (N. Virginia)",
        "locationType": "AWS Region",
        "usagetype": "USE1-AmazonEKS-Hours:extendedSupport",
        "operation": "ExtendedSupport"
      }
    },
    "OUTPOSTS1": {
      "sku": "OUTPOSTS1",
      "productFamily": "Compute",
      "attributes": {
        "servicecode": "AmazonEKS",
        "location": "US East (N. Virginia)",
        "locationType": "AWS Outposts",
        "usagetype": "USE1-AmazonEKS-Local-Outposts-Hours:perCluster",
        "operation": "CreateOperation"
      }
    }
  },
  "terms": {
    "OnDemand": {
      "STANDARD1": {
        "STANDARD1.TERM": {
          "offerTermCode": "TERM",
          "priceDimensions": {
            "STANDARD1.TERM.DIM": {"unit": "Hours", "pricePerUnit": {"USD": "0.1000000000"}, "description": "Amazon EKS cluster usage"}
          }
        }
      },
      "EXTSUPPORT1": {
        "EXTSUPPORT1.TERM": {
          "offerTermCode": "TERM",
          "priceDimensions": {
            "EXTSUPPORT1.TERM.DIM": {"unit": "Hours", "pricePerUnit": {"USD": "0.5000000000"}, "description": "Amazon EKS extended support usage"}
          }
        }
      },
      "OUTPOSTS1": {
        "OUTPOSTS1.TERM": {
          "offerTermCode": "TERM",
          "priceDimensions": {
            "OUTPOSTS1.TERM.DIM": {"unit": "Hours", "pricePerUnit": {"USD": "0.1000000000"}, "description": "Amazon EKS Outposts cluster usage"}
          }
        }
      }
    },
    "Reserved": {}
  }
}`

// TestGetEKSPrice_StandardAndExtendedSupport verifies that GetEKSPrice
// returns exactly the standard per-cluster fee and the extended-support
// surcharge, excluding the Outposts SKU that shares operation=CreateOperation
// with the standard fee but has a different locationType.
func TestGetEKSPrice_StandardAndExtendedSupport(t *testing.T) {
	server := newBulkTestServer(t, []byte(eksBulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := newTestProvider(t)
	p.bulkFallback = true

	prices, err := p.GetEKSPrice(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("GetEKSPrice returned error: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected 2 prices (standard + extended support), got %d: %+v", len(prices), prices)
	}

	byPrice := map[float64]models.NormalizedPrice{}
	for _, np := range prices {
		byPrice[np.PricePerUnit] = np
	}

	standard, ok := byPrice[0.10]
	if !ok {
		t.Fatalf("expected a $0.10/hr standard-tier price, got: %+v", prices)
	}
	if standard.Service != "eks" {
		t.Errorf("expected service=eks, got %q", standard.Service)
	}

	if _, ok := byPrice[0.50]; !ok {
		t.Fatalf("expected a $0.50/hr extended-support price, got: %+v", prices)
	}
}

// TestGetEKSPrice_NoResults verifies that an unmatched region/fixture
// produces a clear error rather than a silent empty-price success.
func TestGetEKSPrice_NoResults(t *testing.T) {
	server := newBulkTestServer(t, []byte(`{"products":{},"terms":{"OnDemand":{},"Reserved":{}}}`), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := newTestProvider(t)
	p.bulkFallback = true

	_, err := p.GetEKSPrice(context.Background(), "us-east-1")
	if err == nil {
		t.Fatal("expected an error when no EKS control-plane SKUs are found, got nil")
	}
}
