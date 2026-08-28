package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/trucore-ai/provengraph/internal/api"
	"github.com/trucore-ai/provengraph/internal/config"
	"github.com/trucore-ai/provengraph/internal/graph"
	"github.com/trucore-ai/provengraph/internal/health"
	"github.com/trucore-ai/provengraph/internal/store"
)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	root := &cobra.Command{
		Use:   "provengraph",
		Short: "ProvenGraph Trust — the provenance graph for the agent economy",
		Long:  "ProvenGraph Trust CLI: serve, doctor, setup, register, resolve, and provenance-sync.\n\nTrust scores for MCP servers, computed over a provenance graph.",
	}

	root.AddCommand(serveCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(setupCmd())
	root.AddCommand(registerCmd())
	root.AddCommand(resolveCmd())
	root.AddCommand(provenanceSyncCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ----- serve -----

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the ProvenGraph Trust API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()

			s, err := store.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer s.Close()

			apiSrv := api.NewServer(s, cfg)

			// Start health checker in background
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go health.Start(ctx, s, &health.Config{
				Interval: cfg.ProbeInterval,
				Timeout:  cfg.ProbeTimeout,
				Workers:  cfg.Workers,
			})

			hs := &http.Server{
				Addr:    cfg.Port,
				Handler: apiSrv,
			}

			// Graceful shutdown
			go func() {
				sigCh := make(chan os.Signal, 1)
				signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
				<-sigCh
				logger.Info("shutting down...")
				cancel()
				shutdownCtx, sdCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer sdCancel()
				hs.Shutdown(shutdownCtx)
			}()

			logger.Info("ProvenGraph Trust starting", "port", cfg.Port, "db", cfg.DBPath)
			fmt.Fprintf(os.Stderr, "🚀 ProvenGraph Trust starting on %s (db: %s)\n", cfg.Port, cfg.DBPath)
			if err := hs.ListenAndServe(); err != http.ErrServerClosed {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}
}

// ----- doctor -----

func doctorCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks (DB, port, connectivity)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			results := map[string]any{"status": "ok", "checks": []map[string]any{}}
			checks := []map[string]any{}

			// Check DB
			s, err := store.Open(cfg.DBPath)
			if err != nil {
				checks = append(checks, map[string]any{"check": "database", "status": "fail", "detail": err.Error()})
				results["status"] = "fail"
			} else {
				checks = append(checks, map[string]any{"check": "database", "status": "ok", "detail": cfg.DBPath})
				// Also check stats
				stats, _ := s.GetStats()
				if stats != nil {
					checks = append(checks, map[string]any{
						"check":  "servers_active",
						"status": "ok",
						"detail": fmt.Sprintf("%d active, %d total", stats.ServersActive, stats.ServersTotal),
					})
				}
				s.Close()
			}

			// Check port
			ln, err := net.Listen("tcp", cfg.Port)
			if err != nil {
				checks = append(checks, map[string]any{"check": "port", "status": "fail", "detail": err.Error()})
				results["status"] = "fail"
			} else {
				ln.Close()
				checks = append(checks, map[string]any{"check": "port", "status": "ok", "detail": cfg.Port})
			}

			results["checks"] = checks

			if jsonOutput {
				out, _ := json.MarshalIndent(results, "", "  ")
				fmt.Println(string(out))
			} else {
				for _, c := range checks {
					icon := "✅"
					if c["status"] == "fail" {
						icon = "❌"
					}
					fmt.Printf("%s %s: %s (%s)\n", icon, c["check"], c["status"], c["detail"])
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output format")
	return cmd
}

// ----- setup -----

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "First-run setup (creates config + DB)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()

			s, err := store.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("failed to initialize database: %w", err)
			}
			defer s.Close()

			fmt.Printf("✅ MeshDNS database created at %s\n", cfg.DBPath)
			fmt.Printf("   Port: %s\n", cfg.Port)
			fmt.Printf("   Run 'meshdns serve' to start.\n")
			return nil
		},
	}
}

// ----- register -----

var (
	regName      string
	regDesc      string
	regServerURL string
	regHealthURL string
	regCaps      []string
	regContact   string
	regBaseURL   string
	regJSON      bool
)

func registerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register a server via CLI",
		RunE: func(cmd *cobra.Command, args []string) error {
			if regName == "" || regServerURL == "" || len(regCaps) == 0 {
				return fmt.Errorf("--name, --server-url, and --caps are required")
			}

			type regReq struct {
				Name         string   `json:"name"`
				Description  string   `json:"description"`
				ServerURL    string   `json:"server_url"`
				HealthURL    string   `json:"health_url"`
				Capabilities []string `json:"capabilities"`
				OwnerContact string   `json:"owner_contact"`
			}

			if regBaseURL == "" {
				regBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(regReq{
				Name: regName, Description: regDesc, ServerURL: regServerURL,
				HealthURL: regHealthURL, Capabilities: regCaps, OwnerContact: regContact,
			})

			resp, err := http.Post(
				strings.TrimRight(regBaseURL, "/")+"/v0/servers",
				"application/json",
				strings.NewReader(string(body)),
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if regJSON {
				out, _ := json.MarshalIndent(result, "", "  ")
				fmt.Println(string(out))
			} else {
				if errData, ok := result["error"]; ok {
					fmt.Printf("❌ Error: %v\n", errData)
				} else {
					fmt.Printf("✅ Registered!\n   Server ID: %v\n   Write Key: %v\n   SAVE YOUR WRITE KEY — it cannot be recovered.\n",
						result["server_id"], result["write_key"])
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&regName, "name", "", "Server name")
	cmd.Flags().StringVar(&regDesc, "description", "", "Server description")
	cmd.Flags().StringVar(&regServerURL, "server-url", "", "Server URL")
	cmd.Flags().StringVar(&regHealthURL, "health-url", "", "Health check URL")
	cmd.Flags().StringSliceVar(&regCaps, "caps", nil, "Capabilities (comma-separated)")
	cmd.Flags().StringVar(&regContact, "contact", "", "Owner contact email")
	cmd.Flags().StringVar(&regBaseURL, "base-url", "http://localhost:8080", "MeshDNS base URL")
	cmd.Flags().BoolVar(&regJSON, "json", false, "JSON output")
	return cmd
}

// ----- resolve -----

var (
	resolveCap  string
	resolveURL  string
	resolveJSON bool
)

func resolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve a capability via CLI",
		RunE: func(cmd *cobra.Command, args []string) error {
			if resolveCap == "" {
				return fmt.Errorf("--capability is required")
			}
			if resolveURL == "" {
				resolveURL = "http://localhost:8080"
			}

			resp, err := http.Get(
				strings.TrimRight(resolveURL, "/") + "/v0/resolve?capability=" + resolveCap,
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var results []map[string]any
			json.NewDecoder(resp.Body).Decode(&results)

			if resolveJSON {
				out, _ := json.MarshalIndent(results, "", "  ")
				fmt.Println(string(out))
			} else {
				if len(results) == 0 {
					fmt.Printf("No servers found for capability: %s\n", resolveCap)
				} else {
					fmt.Printf("Servers for '%s':\n", resolveCap)
					for _, s := range results {
						status := "DOWN"
						if up, ok := s["up"].(float64); ok && up == 1 {
							status = "UP"
						}
						fmt.Printf("  %s %s (%s, %.1f%% uptime)\n",
							iconForStatus(status), s["name"], s["server_url"],
							floatOrZero(s["uptime_30d"])*100)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&resolveCap, "capability", "", "Capability to resolve")
	cmd.Flags().StringVar(&resolveURL, "base-url", "http://localhost:8080", "MeshDNS base URL")
	cmd.Flags().BoolVar(&resolveJSON, "json", false, "JSON output")
	return cmd
}

func iconForStatus(s string) string {
	if s == "UP" {
		return "🟢"
	}
	return "🔴"
}

func floatOrZero(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// ----- provenance-sync -----

// provenanceSyncCmd backfills all active servers into the ProvenGraph core.
// Safe to re-run (upserts + deterministic attestation edges are idempotent).
func provenanceSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "provenance-sync",
		Short: "Backfill all active servers into the ProvenGraph core",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			s, err := store.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer s.Close()

			g := graph.New(s.DB())
			if err := g.Migrate(); err != nil {
				return fmt.Errorf("migrate graph: %w", err)
			}

			n, err := api.SyncAllToGraph(s, g)
			if err != nil {
				return fmt.Errorf("sync: %w", err)
			}
			fmt.Printf("ProvenGraph sync complete: %d servers written to the graph.\n", n)
			return nil
		},
	}
}

func init() {
	// Ensure dependency imports are used
	_ = uuid.New()
	_ = sha256.Sum256(nil)
	_ = hex.EncodeToString(nil)
}