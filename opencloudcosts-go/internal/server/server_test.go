package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/server"
)

// snapshotTool is the parsed representation of one tool entry in
// schemas/tools-snapshot.json.
type snapshotTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// newTestServer returns an AppServer wired with an empty cache and no
// providers — sufficient for schema registration tests.
func newTestServer(t *testing.T) *server.AppServer {
	t.Helper()
	cfg := &config.Config{}
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	return server.New(cfg, cm, nil)
}

// loadSnapshot reads schemas/tools-snapshot.json from the repo root and
// returns a map of tool name → snapshotTool.
func loadSnapshot(t *testing.T) map[string]snapshotTool {
	t.Helper()
	// Navigate from internal/server to the repo root.
	snapshotPath := filepath.Join("..", "..", "schemas", "tools-snapshot.json")
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("cannot read tools-snapshot.json: %v", err)
	}

	var snap struct {
		Tools []snapshotTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("cannot unmarshal tools-snapshot.json: %v", err)
	}

	result := make(map[string]snapshotTool, len(snap.Tools))
	for _, tl := range snap.Tools { // tl avoids shadowing *testing.T
		result[tl.Name] = tl
	}
	return result
}

// connectInMemory creates an in-memory MCP client session connected to the
// given mcp.Server. The returned session is already initialised.
func connectInMemory(t *testing.T, mcpSrv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := mcpSrv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	sess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestAllToolsPresent verifies all 15 expected tool names are present and that
// the count matches the snapshot.
func TestAllToolsPresent(t *testing.T) {
	srv := newTestServer(t)
	sess := connectToServer(t, srv)
	ctx := context.Background()

	tools := collectTools(t, ctx, sess)
	snapshot := loadSnapshot(t)

	if got, want := len(tools), len(snapshot); got != want {
		t.Errorf("tool count: registered=%d, snapshot=%d", got, want)
	}

	// Names are driven from the snapshot — no second hand-typed list.
	for name := range snapshot {
		if _, ok := tools[name]; !ok {
			t.Errorf("missing tool: %q (present in snapshot, not registered)", name)
		}
	}
	for name := range tools {
		if _, ok := snapshot[name]; !ok {
			t.Errorf("extra tool: %q (registered, not in snapshot)", name)
		}
	}
}

// TestSchemaParityWithSnapshot verifies that the InputSchema for each of the
// 15 registered tools is structurally identical to the Phase 0 Python snapshot.
// Comparison is deep-equal on the parsed JSON object, not byte comparison.
func TestSchemaParityWithSnapshot(t *testing.T) {
	srv := newTestServer(t)
	sess := connectToServer(t, srv)
	ctx := context.Background()

	tools := collectTools(t, ctx, sess)
	snapshot := loadSnapshot(t)

	for name, snap := range snapshot {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("tool %q: in snapshot but not registered", name)
			continue
		}

		// Normalise both sides via JSON round-trip to comparable map[string]any.
		gotJSON, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Errorf("tool %q: cannot marshal registered InputSchema: %v", name, err)
			continue
		}
		var got any
		if err := json.Unmarshal(gotJSON, &got); err != nil {
			t.Errorf("tool %q: cannot unmarshal registered InputSchema: %v", name, err)
			continue
		}

		if !reflect.DeepEqual(got, snap.InputSchema) {
			t.Errorf("tool %q: InputSchema drift\ngot:  %s\nwant: %s",
				name, mustMarshalIndent(got), mustMarshalIndent(snap.InputSchema))
		}
	}
}

// TestDescriptionParityWithSnapshot verifies that tool descriptions are
// byte-identical to the Python snapshot. Description drift changes harness
// behaviour because the LLM picks tools based on descriptions.
func TestDescriptionParityWithSnapshot(t *testing.T) {
	srv := newTestServer(t)
	sess := connectToServer(t, srv)
	ctx := context.Background()

	tools := collectTools(t, ctx, sess)
	snapshot := loadSnapshot(t)

	for name, snap := range snapshot {
		tool, ok := tools[name]
		if !ok {
			continue // already reported by TestAllToolsPresent
		}
		if tool.Description != snap.Description {
			t.Errorf("tool %q: description drift\n got:  %q\nwant: %q",
				name, tool.Description, snap.Description)
		}
	}
}

// TestHandlersReturnStructuredJSON verifies that each registered tool handler
// returns valid JSON text content when called with a no-providers server.
// This exercises the real handler path (not stubs): with no providers configured,
// provider-specific tools return structured error objects; cache/describe tools
// return their real responses. All responses must be valid JSON objects.
func TestHandlersReturnStructuredJSON(t *testing.T) {
	srv := newTestServer(t)
	sess := connectToServer(t, srv)
	ctx := context.Background()

	// Call each tool with a minimal valid payload (the required fields from
	// the schema). With no providers configured, provider-specific tools return
	// a structured error object, not a crash. Cache and describe tools return
	// their real responses.
	calls := []struct {
		name string
		args map[string]any
	}{
		{"get_price", map[string]any{"spec": map[string]any{"provider": "aws", "domain": "compute"}}},
		{"get_prices_batch", map[string]any{"provider": "aws", "instance_types": []string{"m5.xlarge"}, "region": "us-east-1"}},
		{"compare_prices", map[string]any{"spec": map[string]any{"provider": "aws"}, "regions": []string{"us-east-1"}}},
		{"search_pricing", map[string]any{"provider": "aws", "query": "m5"}},
		{"get_discount_summary", map[string]any{}},
		{"get_spot_history", map[string]any{"spec": map[string]any{"provider": "aws", "domain": "compute"}}},
		{"refresh_cache", map[string]any{}},
		{"list_regions", map[string]any{"provider": "aws"}},
		{"list_instance_types", map[string]any{"provider": "aws", "region": "us-east-1"}},
		{"describe_catalog", map[string]any{}},
		{"find_cheapest_region", map[string]any{"spec": map[string]any{"provider": "aws"}}},
		{"find_available_regions", map[string]any{"spec": map[string]any{"provider": "aws"}}},
		{"cache_stats", map[string]any{}},
		{"estimate_bom", map[string]any{"items": []any{}}},
		{"estimate_unit_economics", map[string]any{"items": []any{}, "units_per_month": 100.0}},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{
				Name:      tc.name,
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool(%q): %v", tc.name, err)
			}
			// IsError is acceptable — providers return structured errors when not configured.
			if len(res.Content) == 0 {
				t.Errorf("CallTool(%q): no content in response", tc.name)
				return
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Errorf("CallTool(%q): content[0] is %T, not *TextContent", tc.name, res.Content[0])
				return
			}
			// Response must be valid JSON.
			var payload map[string]any
			if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
				t.Errorf("CallTool(%q): response is not valid JSON: %v\nresponse: %s",
					tc.name, err, text.Text)
			}
		})
	}
}

// TestHealthz verifies the /healthz endpoint returns 200 with the expected body.
func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	handler := buildHTTPHandler(srv)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Errorf("/healthz: status %d, want 200", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz: body not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`/healthz: "status"=%q, want "ok"`, body["status"])
	}
	if body["version"] != "dev" {
		t.Errorf(`/healthz: "version"=%q, want "dev"`, body["version"])
	}
}

// TestReadyzNoProviders verifies /readyz returns 503 when no providers are
// configured (the test server is built with an empty provider map).
func TestReadyzNoProviders(t *testing.T) {
	srv := newTestServer(t)
	handler := buildHTTPHandler(srv)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// With no providers, readyz must return 503.
	if got := rec.Code; got != http.StatusServiceUnavailable {
		t.Errorf("/readyz (no providers): status %d, want 503", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/readyz: body not JSON: %v", err)
	}
	if body["status"] != "not_ready" {
		t.Errorf(`/readyz: "status"=%q, want "not_ready"`, body["status"])
	}
}

// ---- helpers ----

// connectToServer constructs an in-memory MCP session connected to the
// AppServer's registered tools.
func connectToServer(t *testing.T, srv *server.AppServer) *mcp.ClientSession {
	t.Helper()
	mcpSrv := srv.BuildMCPServerForTest()
	return connectInMemory(t, mcpSrv)
}

// buildHTTPHandler returns the http.ServeMux that RunHTTP would use.
func buildHTTPHandler(srv *server.AppServer) http.Handler {
	return srv.BuildHTTPHandlerForTest()
}

// collectTools returns a map of tool name → *mcp.Tool by listing via the session.
func collectTools(t *testing.T, ctx context.Context, sess *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	tools := make(map[string]*mcp.Tool)
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("sess.Tools: %v", err)
		}
		cp := *tool
		tools[tool.Name] = &cp
	}
	return tools
}

func mustMarshalIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	return string(b)
}
