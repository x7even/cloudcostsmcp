// Command opencloudcosts is the MCP server binary for OpenCloudCosts.
//
// Usage:
//
//	opencloudcosts                              # stdio (default)
//	opencloudcosts --transport http             # streamable-HTTP on port 8080
//	opencloudcosts --transport http --host 0.0.0.0 --port 9000
//	opencloudcosts --help
//
// Environment overrides (mirroring the Python implementation):
//
//	OCC_HTTP_HOST   — default bind host for HTTP transport (default: 127.0.0.1)
//	OCC_HTTP_PORT   — default bind port for HTTP transport (default: 8080)
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	azureprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/azure"
	gcpprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/gcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/server"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

// version is the build-time version string, overridden by:
//
//	go build -ldflags="-X main.version=$(git describe --tags)"
var version = "dev"

func main() {
	// Parse flags with the same env-var precedence as the Python implementation:
	// flag value > env var > built-in default.
	fs := flag.NewFlagSet("opencloudcosts", flag.ExitOnError)

	transport := fs.String("transport", "stdio", "Transport type: stdio or http")
	host := fs.String("host", envString("OCC_HTTP_HOST", "127.0.0.1"),
		"HTTP bind address (default: 127.0.0.1, env: OCC_HTTP_HOST)")
	port := fs.String("port", envString("OCC_HTTP_PORT", "8080"),
		"HTTP port (default: 8080, env: OCC_HTTP_PORT)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "opencloudcosts %s — MCP server for cloud pricing\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: opencloudcosts [flags]\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		// ExitOnError means this won't be reached, but keep for clarity.
		fmt.Fprintf(os.Stderr, "flag error: %v\n", err)
		os.Exit(1)
	}

	// Handle --version separately (before loading config).
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			fmt.Println("opencloudcosts", version)
			os.Exit(0)
		}
	}

	// Validate transport choice.
	switch *transport {
	case "stdio", "http":
	default:
		fmt.Fprintf(os.Stderr, "error: unknown transport %q — must be stdio or http\n", *transport)
		os.Exit(1)
	}

	// Validate port is numeric.
	if _, err := strconv.Atoi(*port); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid port %q: %v\n", *port, err)
		os.Exit(1)
	}

	// Initialise config.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	// Configure structured JSON logging via log/slog.
	// For stdio transport: log at warn+ to avoid polluting stdout/stderr with
	// noisy debug lines that could interfere with MCP protocol framing.
	// For HTTP transport: honour OCC_LOG_LEVEL (default info).
	logLevel := slogLevel(cfg.LogLevel)
	if *transport == "stdio" && logLevel < slog.LevelWarn {
		logLevel = slog.LevelWarn
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("starting opencloudcosts",
		"version", version,
		"transport", *transport,
	)

	// Initialise cache.
	cm, err := cache.New(cfg.CacheDir)
	if err != nil {
		// Non-fatal: start with an empty in-memory cache.
		slog.Error("cache init failed, starting empty", "error", err)
		cacheDir, _ := cache.DefaultCacheDir()
		if cm2, err2 := cache.New(cacheDir); err2 == nil {
			cm = cm2
		}
	}
	if cm == nil {
		// Absolute fallback: temp dir cache.
		tmp := os.TempDir()
		cm, _ = cache.New(tmp)
	}

	// Initialise providers with graceful degradation:
	// if a provider fails to init (bad credentials, network error), log the
	// failure and continue serving the remaining providers. Failed providers
	// are not added to the map, so tool calls for them return structured errors.
	provs := initProviders(cfg, cm)
	if len(provs) == 0 {
		slog.Warn("no providers initialised — all provider-specific tool calls will return errors")
	}

	// Build the application server.
	srv := server.New(cfg, cm, provs)

	if err := runServer(srv, *transport, *host, *port, cfg); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// runServer starts the appropriate transport and blocks until it exits.
// It owns the signal context so defers run cleanly before returning.
func runServer(srv *server.AppServer, transport, host, port string, cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	switch transport {
	case "http":
		slog.Info("starting HTTP server", "host", host, "port", port, "version", version)
		errCh := make(chan error, 1)
		go func() {
			errCh <- srv.RunHTTP(host, port)
		}()
		select {
		case <-ctx.Done():
			slog.Info("SIGTERM received, draining in-flight requests",
				"shutdown_timeout", cfg.ShutdownTimeout.String())
			// Give in-flight requests time to finish.
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer shutdownCancel()
			<-shutdownCtx.Done()
			slog.Info("shutdown complete")
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("HTTP server: %w", err)
			}
		}
	default:
		// stdio — standard for MCP clients (Claude Code, etc.)
		// Stdio doesn't use the signal context for shutdown because the MCP
		// client controls the process lifecycle (closes stdin to signal EOF).
		if err := srv.RunStdio(); err != nil {
			return fmt.Errorf("stdio server: %w", err)
		}
	}
	return nil
}

// initProviders attempts to initialise each cloud provider and returns the map
// of successfully constructed providers. Failed providers are logged at warn
// level and excluded from the map — the server continues without them.
func initProviders(cfg *config.Config, cm *cache.CacheManager) map[string]providers.Provider {
	provs := make(map[string]providers.Provider)

	// AWS — always attempt; credentials are optional for public pricing.
	awsProv, err := awsprovider.NewProvider(cfg, cm)
	if err != nil {
		slog.Warn("AWS provider init failed — AWS tools will return errors",
			"error", err)
	} else {
		provs["aws"] = awsProv
		slog.Info("AWS provider initialised")
	}

	// GCP — always attempt; API key is optional for public pricing.
	gcpProv, err := gcpprovider.NewProvider(cfg, cm)
	if err != nil {
		slog.Warn("GCP provider init failed — GCP tools will return errors",
			"error", err)
	} else {
		provs["gcp"] = gcpProv
		slog.Info("GCP provider initialised")
	}

	// Azure — always succeeds (fully public API, no credentials needed).
	azureProv := azureprovider.NewProvider(
		cm,
		cfg.CacheTTL(),
		cfg.MetadataTTL(),
	)
	provs["azure"] = azureProv
	slog.Info("Azure provider initialised")

	return provs
}

// slogLevel converts an OCC_LOG_LEVEL string to a slog.Level.
// Unknown values default to slog.LevelInfo.
func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// envString returns the value of the environment variable name, or fallback
// if the variable is unset or empty. This mirrors the Python argument-parser
// pattern: os.getenv("OCC_HTTP_HOST", "127.0.0.1").
func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
