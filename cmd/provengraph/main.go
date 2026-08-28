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
	root.AddCommand(knowledgeCreateCmd())
	root.AddCommand(knowledgeGetCmd())
	root.AddCommand(knowledgeListCmd())
	root.AddCommand(knowledgeSupersedeCmd())
	root.AddCommand(knowledgeContradictCmd())
	root.AddCommand(knowledgeAttestCmd())
	root.AddCommand(memoryCreateCmd())
	root.AddCommand(memoryGetCmd())
	root.AddCommand(memoryListCmd())
	root.AddCommand(memoryRememberCmd())
	root.AddCommand(memoryForgetCmd())
	root.AddCommand(memoryDeleteCmd())

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

// ----- knowledge-create -----

var (
	kCreateContent string
	kCreateDomain  string
	kCreateIssuer  string
	kBaseURL       string
)

func knowledgeCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-create",
		Short: "Create a knowledge claim",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kCreateContent == "" || kCreateDomain == "" {
				return fmt.Errorf("--content and --domain are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{
				"content": kCreateContent,
				"domain":  kCreateDomain,
				"issuer":  kCreateIssuer,
			})

			resp, err := http.Post(
				strings.TrimRight(kBaseURL, "/")+"/v0/knowledge",
				"application/json",
				strings.NewReader(string(body)),
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Claim created!\n   Claim ID: %v\n   Write Key: %v\n   SAVE YOUR WRITE KEY.\n",
					result["claim_id"], result["write_key"])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kCreateContent, "content", "", "Claim content")
	cmd.Flags().StringVar(&kCreateDomain, "domain", "", "Claim domain")
	cmd.Flags().StringVar(&kCreateIssuer, "issuer", "", "Issuer name")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- knowledge-get -----

var kGetID string

func knowledgeGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-get",
		Short: "Get a knowledge claim by ID",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kGetID == "" {
				return fmt.Errorf("--id is required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			resp, err := http.Get(strings.TrimRight(kBaseURL, "/") + "/v0/knowledge/" + kGetID)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
				return nil
			}
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&kGetID, "id", "", "Claim ID")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- knowledge-list -----

var (
	kListDomain string
	kListQuery  string
)

func knowledgeListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-list",
		Short: "List knowledge claims",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			url := strings.TrimRight(kBaseURL, "/") + "/v0/knowledge"
			params := []string{}
			if kListDomain != "" {
				params = append(params, "domain="+kListDomain)
			}
			if kListQuery != "" {
				params = append(params, "q="+kListQuery)
			}
			if len(params) > 0 {
				url += "?" + strings.Join(params, "&")
			}

			resp, err := http.Get(url)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
				return nil
			}
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&kListDomain, "domain", "", "Filter by domain")
	cmd.Flags().StringVar(&kListQuery, "query", "", "Search query")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- knowledge-supersede -----

var (
	kSupersedeID  string
	kSupersedesID string
	kSupersedeKey string
)

func knowledgeSupersedeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-supersede",
		Short: "Assert one claim supersedes another",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kSupersedeID == "" || kSupersedesID == "" || kSupersedeKey == "" {
				return fmt.Errorf("--id, --supersedes, and --write-key are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{"supersedes_id": kSupersedesID})
			req, _ := http.NewRequest("POST",
				strings.TrimRight(kBaseURL, "/")+"/v0/knowledge/"+kSupersedeID+"/supersede",
				strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Write-Key", kSupersedeKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Superseded claim %s\n", kSupersedesID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kSupersedeID, "id", "", "Claim ID (the superseder)")
	cmd.Flags().StringVar(&kSupersedesID, "supersedes", "", "Claim ID being superseded")
	cmd.Flags().StringVar(&kSupersedeKey, "write-key", "", "Write key")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- knowledge-contradict -----

var (
	kContradictID  string
	kContradictsID string
	kContradictKey string
)

func knowledgeContradictCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-contradict",
		Short: "Assert one claim contradicts another",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kContradictID == "" || kContradictsID == "" || kContradictKey == "" {
				return fmt.Errorf("--id, --contradicts, and --write-key are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{"contradicts_id": kContradictsID})
			req, _ := http.NewRequest("POST",
				strings.TrimRight(kBaseURL, "/")+"/v0/knowledge/"+kContradictID+"/contradict",
				strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Write-Key", kContradictKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Contradicted claim %s\n", kContradictsID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kContradictID, "id", "", "Claim ID (the contradictor)")
	cmd.Flags().StringVar(&kContradictsID, "contradicts", "", "Claim ID being contradicted")
	cmd.Flags().StringVar(&kContradictKey, "write-key", "", "Write key")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- knowledge-attest -----

var (
	kAttestID     string
	kAttestIssuer string
)

func knowledgeAttestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge-attest",
		Short: "Attest to a knowledge claim",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kAttestID == "" || kAttestIssuer == "" {
				return fmt.Errorf("--id and --issuer are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{"issuer": kAttestIssuer})
			resp, err := http.Post(
				strings.TrimRight(kBaseURL, "/")+"/v0/knowledge/"+kAttestID+"/attest",
				"application/json",
				strings.NewReader(string(body)),
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Attested to claim %s as %s\n", kAttestID, kAttestIssuer)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kAttestID, "id", "", "Claim ID")
	cmd.Flags().StringVar(&kAttestIssuer, "issuer", "", "Issuer name")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-create -----

var (
	mCreateContent   string
	mCreateCategory  string
	mCreateRetention string
	mCreatePurpose   string
	mCreateSubject   string
	mCreateOwner     string
)

func memoryCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-create",
		Short: "Create a memory entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if mCreateContent == "" || mCreateOwner == "" {
				return fmt.Errorf("--content and --owner are required")
			}
			if mCreateRetention == "" {
				mCreateRetention = "permanent"
			}
			if mCreateCategory == "" {
				mCreateCategory = "fact"
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{
				"content":   mCreateContent,
				"category":  mCreateCategory,
				"retention": mCreateRetention,
				"purpose":   mCreatePurpose,
				"subject":   mCreateSubject,
				"owner":     mCreateOwner,
			})

			resp, err := http.Post(
				strings.TrimRight(kBaseURL, "/")+"/v0/memory",
				"application/json",
				strings.NewReader(string(body)),
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Memory created!\n   Memory ID: %v\n   Write Key: %v\n   SAVE YOUR WRITE KEY.\n",
					result["memory_id"], result["write_key"])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mCreateContent, "content", "", "Memory content")
	cmd.Flags().StringVar(&mCreateCategory, "category", "fact", "Category")
	cmd.Flags().StringVar(&mCreateRetention, "retention", "permanent", "Retention policy")
	cmd.Flags().StringVar(&mCreatePurpose, "purpose", "", "Purpose of storage")
	cmd.Flags().StringVar(&mCreateSubject, "subject", "", "Subject DID/agent")
	cmd.Flags().StringVar(&mCreateOwner, "owner", "", "Owner agent")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-get -----

var mGetID string

func memoryGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-get",
		Short: "Get a memory entry by ID",
		RunE: func(cmd *cobra.Command, args []string) error {
			if mGetID == "" {
				return fmt.Errorf("--id is required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			resp, err := http.Get(strings.TrimRight(kBaseURL, "/") + "/v0/memory/" + mGetID)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
				return nil
			}
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&mGetID, "id", "", "Memory ID")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-list -----

var (
	mListAgent    string
	mListCategory string
	mListMemQuery string
)

func memoryListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-list",
		Short: "List memory entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			url := strings.TrimRight(kBaseURL, "/") + "/v0/memory"
			params := []string{}
			if mListAgent != "" {
				params = append(params, "agent="+mListAgent)
			}
			if mListCategory != "" {
				params = append(params, "category="+mListCategory)
			}
			if mListMemQuery != "" {
				params = append(params, "q="+mListMemQuery)
			}
			if len(params) > 0 {
				url += "?" + strings.Join(params, "&")
			}

			resp, err := http.Get(url)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
				return nil
			}
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&mListAgent, "agent", "", "Filter by agent ID")
	cmd.Flags().StringVar(&mListCategory, "category", "", "Filter by category")
	cmd.Flags().StringVar(&mListMemQuery, "query", "", "Search query")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-remember -----

var (
	mRememberID    string
	mRememberAgent string
)

func memoryRememberCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-remember",
		Short: "Agent remembers a memory entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if mRememberID == "" || mRememberAgent == "" {
				return fmt.Errorf("--id and --agent are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			body, _ := json.Marshal(map[string]string{"agent_id": mRememberAgent})
			resp, err := http.Post(
				strings.TrimRight(kBaseURL, "/")+"/v0/memory/"+mRememberID+"/remember",
				"application/json",
				strings.NewReader(string(body)),
			)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Agent %s now remembers memory %s\n", mRememberAgent, mRememberID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mRememberID, "id", "", "Memory ID")
	cmd.Flags().StringVar(&mRememberAgent, "agent", "", "Agent ID")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-forget -----

var (
	mForgetID    string
	mForgetAgent string
)

func memoryForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-forget",
		Short: "Agent forgets a memory entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if mForgetID == "" || mForgetAgent == "" {
				return fmt.Errorf("--id and --agent are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			url := fmt.Sprintf("%s/v0/memory/%s/forget?agent=%s",
				strings.TrimRight(kBaseURL, "/"), mForgetID, mForgetAgent)
			req, _ := http.NewRequest("DELETE", url, nil)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Agent %s forgot memory %s\n", mForgetAgent, mForgetID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mForgetID, "id", "", "Memory ID")
	cmd.Flags().StringVar(&mForgetAgent, "agent", "", "Agent ID")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

// ----- memory-delete -----

var (
	mDeleteID  string
	mDeleteKey string
)

func memoryDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-delete",
		Short: "Delete a memory entry (right to be forgotten)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if mDeleteID == "" || mDeleteKey == "" {
				return fmt.Errorf("--id and --write-key are required")
			}
			if kBaseURL == "" {
				kBaseURL = "http://localhost:8080"
			}

			req, _ := http.NewRequest("DELETE",
				strings.TrimRight(kBaseURL, "/")+"/v0/memory/"+mDeleteID, nil)
			req.Header.Set("X-Write-Key", mDeleteKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			var result map[string]any
			json.NewDecoder(resp.Body).Decode(&result)

			if errData, ok := result["error"]; ok {
				fmt.Printf("❌ Error: %v\n", errData)
			} else {
				fmt.Printf("✅ Memory %s deleted\n", mDeleteID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mDeleteID, "id", "", "Memory ID")
	cmd.Flags().StringVar(&mDeleteKey, "write-key", "", "Write key")
	cmd.Flags().StringVar(&kBaseURL, "base-url", "http://localhost:8080", "ProvenGraph base URL")
	return cmd
}

func init() {
	// Ensure dependency imports are used
	_ = uuid.New()
	_ = sha256.Sum256(nil)
	_ = hex.EncodeToString(nil)
}