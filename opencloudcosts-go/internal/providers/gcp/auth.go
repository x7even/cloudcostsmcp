// Package gcp implements the GCP cloud pricing provider.
package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
)

const (
	billingReadonlyScope = "https://www.googleapis.com/auth/cloud-billing.readonly"
	maxJSONBytes         = 65536 // 64 KiB
)

var rawTokenWarning = "OCC_GCP_ACCESS_TOKEN is set. Raw Bearer tokens expire after ~1 hour and are " +
	"NOT suitable for long-running MCP servers. Use OCC_GCP_SERVICE_ACCOUNT_JSON_B64, " +
	"GOOGLE_APPLICATION_CREDENTIALS, or a GCP metadata-server credential instead."

// ErrNotConfigured is returned when required credentials are missing or invalid.
type ErrNotConfigured struct {
	Msg string
}

func (e *ErrNotConfigured) Error() string { return e.Msg }

// gcpAuthProvider holds credentials state for GCP API calls.
// It is safe for concurrent use; token refresh is protected by mu.
type gcpAuthProvider struct {
	cfg            *config.Config
	mu             sync.Mutex
	tokenSource    oauth2.TokenSource // nil until initialized
	warnedRawToken bool
	once           sync.Once // guards first build
	buildErr       error     // saved error from first build
}

func newGCPAuthProvider(cfg *config.Config) *gcpAuthProvider {
	return &gcpAuthProvider{cfg: cfg}
}

// getToken returns a valid Bearer token, refreshing via the TokenSource if needed.
// For the raw-token path it returns the static token after checking expiry.
func (a *gcpAuthProvider) getToken(ctx context.Context) (string, error) {
	// Path 1: raw access token — static, no refresh.
	if a.cfg.GCPAccessToken != "" {
		if !a.warnedRawToken {
			slog.Warn(rawTokenWarning)
			a.warnedRawToken = true
		}
		if err := checkRawTokenExpiry(a.cfg.GCPAccessTokenExpiresAt); err != nil {
			return "", err
		}
		return a.cfg.GCPAccessToken, nil
	}

	// Paths 2–4: OAuth2 token source.
	a.once.Do(func() {
		a.tokenSource, a.buildErr = a.buildTokenSource(ctx)
	})
	if a.buildErr != nil {
		return "", a.buildErr
	}
	if a.tokenSource == nil {
		return "", &ErrNotConfigured{Msg: "GCP auth: no credentials available"}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	tok, err := a.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("GCP auth: token refresh failed: %w", err)
	}
	return tok.AccessToken, nil
}

// addAuthHeader adds Authorization or ?key= to an http.Request.
// For the public catalog API the caller may also use API key params directly.
func (a *gcpAuthProvider) addBearerHeader(ctx context.Context, req *http.Request) error {
	tok, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// buildTokenSource constructs an oauth2.TokenSource from the first matching credential.
func (a *gcpAuthProvider) buildTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	// 2a. Service account JSON — B64 variant
	if a.cfg.GCPServiceAccountJSONB64 != "" {
		data, err := decodeB64JSON(a.cfg.GCPServiceAccountJSONB64, "OCC_GCP_SERVICE_ACCOUNT_JSON_B64")
		if err != nil {
			return nil, err
		}
		return jsonTokenSource(ctx, data)
	}

	// 2b. Service account JSON — raw
	if a.cfg.GCPServiceAccountJSON != "" {
		data, err := parseRawJSON(a.cfg.GCPServiceAccountJSON, "OCC_GCP_SERVICE_ACCOUNT_JSON")
		if err != nil {
			return nil, err
		}
		return jsonTokenSource(ctx, data)
	}

	// 3a. External account / WIF — B64 variant
	if a.cfg.GCPExternalAccountJSONB64 != "" {
		data, err := decodeB64JSON(a.cfg.GCPExternalAccountJSONB64, "OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64")
		if err != nil {
			return nil, err
		}
		return jsonTokenSource(ctx, data)
	}

	// 3b. External account / WIF — raw
	if a.cfg.GCPExternalAccountJSON != "" {
		data, err := parseRawJSON(a.cfg.GCPExternalAccountJSON, "OCC_GCP_EXTERNAL_ACCOUNT_JSON")
		if err != nil {
			return nil, err
		}
		return jsonTokenSource(ctx, data)
	}

	// 4. ADC: GOOGLE_APPLICATION_CREDENTIALS / metadata server / gcloud local ADC
	creds, err := google.FindDefaultCredentials(ctx, billingReadonlyScope)
	if err != nil {
		return nil, &ErrNotConfigured{
			Msg: "GCP effective pricing: no credentials found.\n\n" +
				"Supported credential sources (in priority order):\n" +
				"  1. OCC_GCP_SERVICE_ACCOUNT_JSON_B64 — base64-encoded SA key (Docker/K8s)\n" +
				"  2. OCC_GCP_SERVICE_ACCOUNT_JSON    — raw SA key JSON\n" +
				"  3. OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64 — Workload Identity Federation\n" +
				"  4. GOOGLE_APPLICATION_CREDENTIALS  — path to a key or ADC config file\n" +
				"  5. GCP metadata server             — Cloud Run, GKE, GCE attached SA\n" +
				"  6. OCC_GCP_ACCESS_TOKEN            — raw token (debug only, ~1 h)\n\n" +
				"Run 'gcloud auth application-default login' or set one of the env vars above.\n" +
				"Original error: " + err.Error(),
		}
	}
	return oauth2.ReuseTokenSource(nil, creds.TokenSource), nil
}

// jsonTokenSource creates a reusable oauth2.TokenSource from raw JSON credentials bytes.
// google.CredentialsFromJSON dispatches on the "type" field, supporting both
// service_account and external_account (Workload Identity Federation) JSON.
func jsonTokenSource(ctx context.Context, data []byte) (oauth2.TokenSource, error) {
	creds, err := google.CredentialsFromJSON(ctx, data, billingReadonlyScope) //nolint:staticcheck // deprecated but replacement requires google-cloud-go dependency
	if err != nil {
		return nil, fmt.Errorf("GCP auth: credentials from JSON: %w", err)
	}
	return oauth2.ReuseTokenSource(nil, creds.TokenSource), nil
}

// decodeB64JSON base64-decodes a credential string and parses the resulting JSON.
func decodeB64JSON(b64 string, varName string) ([]byte, error) {
	stripped := trimSpace(b64)
	if len(stripped) > maxJSONBytes {
		return nil, &ErrNotConfigured{
			Msg: fmt.Sprintf("%s exceeds maximum allowed size (%d bytes). "+
				"Verify the value is a base64-encoded credential.", varName, maxJSONBytes),
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(stripped)
	if err != nil {
		// Try URL-safe variant
		decoded, err = base64.URLEncoding.DecodeString(stripped)
		if err != nil {
			// Try RawStdEncoding (no padding)
			decoded, err = base64.RawStdEncoding.DecodeString(stripped)
			if err != nil {
				return nil, &ErrNotConfigured{
					Msg: fmt.Sprintf("%s is not valid base64-encoded JSON. Check the encoding.", varName),
				}
			}
		}
	}
	if !json.Valid(decoded) {
		return nil, &ErrNotConfigured{
			Msg: fmt.Sprintf("%s decoded but is not valid JSON. Check the encoding.", varName),
		}
	}
	return decoded, nil
}

// parseRawJSON validates and returns the raw JSON credential string as bytes.
func parseRawJSON(raw string, varName string) ([]byte, error) {
	if len(raw) > maxJSONBytes {
		return nil, &ErrNotConfigured{
			Msg: fmt.Sprintf("%s exceeds maximum allowed size (%d bytes).", varName, maxJSONBytes),
		}
	}
	data := []byte(raw)
	if !json.Valid(data) {
		return nil, &ErrNotConfigured{
			Msg: fmt.Sprintf("%s is not valid JSON. Check the value.", varName),
		}
	}
	return data, nil
}

// checkRawTokenExpiry logs or returns an error if the expiry timestamp is past.
func checkRawTokenExpiry(expiresAt string) error {
	if expiresAt == "" {
		return nil
	}
	// Try ISO-8601 with Z suffix converted to +00:00
	s := expiresAt
	if len(s) > 0 && s[len(s)-1] == 'Z' {
		s = s[:len(s)-1] + "+00:00"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Unparseable — log and continue rather than hard-failing.
		slog.Warn("OCC_GCP_ACCESS_TOKEN_EXPIRES_AT is not a valid ISO-8601 datetime — expiry check skipped",
			"value", expiresAt)
		return nil
	}
	if time.Now().UTC().After(t) {
		return &ErrNotConfigured{
			Msg: fmt.Sprintf("OCC_GCP_ACCESS_TOKEN expired at %s. "+
				"Provide a fresh token or switch to a service-account credential.", expiresAt),
		}
	}
	return nil
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
