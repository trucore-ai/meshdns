package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const defaultBaseURL = "https://meshdns.trucore.xyz"

type exitCode int

const (
	exitOK         exitCode = 0
	exitError      exitCode = 1
	exitAuth       exitCode = 2
	exitInvalid    exitCode = 4
	exitUnknown    exitCode = 1
	requestTimeout          = 10 * time.Second
)

type appConfig struct {
	baseURL     string
	json        bool
	writeKey    string
	force       bool
	command     string
	commandArgs []string
}

type remoteError struct {
	Error struct {
		Code   string      `json:"code"`
		Detail interface{} `json:"detail"`
	} `json:"error"`
}

type registerRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ServerURL    string   `json:"server_url"`
	HealthURL    string   `json:"health_url"`
	ProbeMethod  string   `json:"probe_method"`
	Capabilities []string `json:"capabilities"`
	OwnerContact string   `json:"owner_contact"`
}

type registerResponse struct {
	ServerID string `json:"server_id"`
	WriteKey string `json:"write_key"`
}

type server struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ServerURL    string   `json:"server_url"`
	HealthURL    string   `json:"health_url"`
	ProbeMethod  string   `json:"probe_method"`
	Capabilities []string `json:"capabilities"`
	Status       string   `json:"status"`
	Up           bool     `json:"up"`
	Uptime30d    float64  `json:"uptime_30d"`
	LastChecked  string   `json:"last_checked_at"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type listResponse struct {
	Servers    []server `json:"servers"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type statsResponse struct {
	ServersActive int `json:"servers_active"`
	ServersTotal  int `json:"servers_total"`
	UpCount       int `json:"up_count"`
	Resolutions24 int `json:"resolutions_24h"`
	Probes24      int `json:"probes_24h"`
}

type capabilityInfo struct {
	Name        string `json:"name"`
	ServerCount int    `json:"server_count"`
}

type capabilitiesResponse struct {
	Capabilities []capabilityInfo `json:"capabilities"`
}

func main() {
	os.Exit(int(run(os.Args[1:])))
}

func run(args []string) exitCode {
	cfg, err := parseArgs(args)
	if err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), suggestedCommand(cfg.command))
	}
	if cfg.command == "" {
		printGlobalHelp()
		return exitOK
	}

	client := &http.Client{Timeout: requestTimeout}

	switch cfg.command {
	case "register":
		return cmdRegister(client, cfg)
	case "list":
		return cmdList(client, cfg)
	case "resolve":
		return cmdResolve(client, cfg)
	case "stats":
		return cmdStats(client, cfg)
	case "status":
		return cmdStatus(client, cfg)
	case "delist":
		return cmdDelist(client, cfg)
	case "capabilities":
		return cmdCapabilities(client, cfg)
	case "doctor":
		return cmdDoctor(client, cfg)
	case "help":
		printGlobalHelp()
		return exitOK
	default:
		return reportError(cfg, exitInvalid, "invalid-input", "unknown command: "+cfg.command, "meshdns-cli --help")
	}
}

func parseArgs(args []string) (appConfig, error) {
	cfg := appConfig{
		baseURL:  strings.TrimSpace(os.Getenv("MESHDNS_URL")),
		writeKey: strings.TrimSpace(os.Getenv("MESHDNS_WRITE_KEY")),
	}
	if cfg.baseURL == "" {
		cfg.baseURL = defaultBaseURL
	}

	var cmdArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--help", "-h":
			if cfg.command == "" {
				cfg.command = "help"
			} else {
				cmdArgs = append(cmdArgs, arg)
			}
			continue
		case "--json":
			cfg.json = true
			continue
		case "--force":
			cfg.force = true
			continue
		case "--url":
			if i+1 >= len(args) {
				return cfg, errors.New("--url requires a value")
			}
			i++
			cfg.baseURL = args[i]
			continue
		case "--write-key":
			if i+1 >= len(args) {
				return cfg, errors.New("--write-key requires a value")
			}
			i++
			cfg.writeKey = args[i]
			continue
		}

		if strings.HasPrefix(arg, "--url=") {
			cfg.baseURL = strings.TrimPrefix(arg, "--url=")
			continue
		}
		if strings.HasPrefix(arg, "--write-key=") {
			cfg.writeKey = strings.TrimPrefix(arg, "--write-key=")
			continue
		}
		if cfg.command == "" {
			cfg.command = arg
			continue
		}
		cmdArgs = append(cmdArgs, arg)
	}

	cfg.baseURL = strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	cfg.commandArgs = cmdArgs
	if cfg.baseURL == "" {
		return cfg, errors.New("base URL cannot be empty")
	}
	if _, err := url.ParseRequestURI(cfg.baseURL); err != nil {
		return cfg, fmt.Errorf("invalid --url value: %w", err)
	}
	return cfg, nil
}

func cmdRegister(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printRegisterHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "")
	description := fs.String("description", "", "")
	serverURL := fs.String("server-url", "", "")
	healthURL := fs.String("health-url", "", "")
	caps := fs.String("capabilities", "", "")
	owner := fs.String("owner-contact", "", "")
	probe := fs.String("probe-method", "", "")
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli register --help")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*serverURL) == "" || strings.TrimSpace(*caps) == "" {
		return reportError(cfg, exitInvalid, "invalid-input", "register requires --name, --server-url, and --capabilities", "meshdns-cli register --help")
	}
	req := registerRequest{
		Name:         strings.TrimSpace(*name),
		Description:  strings.TrimSpace(*description),
		ServerURL:    strings.TrimSpace(*serverURL),
		HealthURL:    strings.TrimSpace(*healthURL),
		ProbeMethod:  strings.TrimSpace(*probe),
		Capabilities: splitCSV(*caps),
		OwnerContact: strings.TrimSpace(*owner),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return reportError(cfg, exitError, "internal-error", err.Error(), "meshdns-cli register --help")
	}
	resp, err := doJSON(client, http.MethodPost, cfg.baseURL+"/v0/servers", body, "", "")
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli register --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli register --help")
	}
	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return reportError(cfg, exitError, "internal-error", "invalid response from API", "meshdns-cli register --help")
	}
	fmt.Fprintln(os.Stderr, "SAVE THIS KEY:", out.WriteKey)
	if cfg.json {
		return writeSuccess(cfg, map[string]string{"server_id": out.ServerID, "write_key": out.WriteKey})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"server_id": out.ServerID, "write_key": out.WriteKey})
	return exitOK
}

func cmdList(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printListHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	query := fs.String("query", "", "")
	capability := fs.String("capability", "", "")
	status := fs.String("status", "", "")
	limit := fs.Int("limit", 20, "")
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli list --help")
	}
	values := url.Values{}
	if q := strings.TrimSpace(*query); q != "" {
		values.Set("query", q)
	}
	if c := strings.TrimSpace(*capability); c != "" {
		values.Set("capability", c)
	}
	if s := strings.TrimSpace(*status); s != "" {
		values.Set("status", s)
	}
	if *limit > 0 {
		values.Set("limit", strconv.Itoa(*limit))
	}
	resp, err := doJSON(client, http.MethodGet, cfg.baseURL+"/v0/servers?"+values.Encode(), nil, "", "")
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli list --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli list --help")
	}
	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return reportError(cfg, exitError, "internal-error", "invalid response from API", "meshdns-cli list --help")
	}
	if cfg.json {
		return writeSuccess(cfg, out)
	}
	renderList(out.Servers)
	return exitOK
}

func cmdResolve(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printResolveHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli resolve --help")
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return reportError(cfg, exitInvalid, "invalid-input", "resolve requires exactly one capability", "meshdns-cli resolve --help")
	}
	capability := strings.TrimSpace(rest[0])
	if capability == "" {
		return reportError(cfg, exitInvalid, "invalid-input", "resolve requires exactly one capability", "meshdns-cli resolve --help")
	}
	resp, err := doJSON(client, http.MethodGet, cfg.baseURL+"/v0/resolve?capability="+url.QueryEscape(capability), nil, "", "")
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli resolve --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli resolve --help")
	}
	var servers []server
	if err := json.NewDecoder(resp.Body).Decode(&servers); err != nil {
		return reportError(cfg, exitError, "internal-error", "invalid response from API", "meshdns-cli resolve --help")
	}
	if cfg.json {
		return writeSuccess(cfg, servers)
	}
	renderResolve(servers)
	return exitOK
}

func cmdStats(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printStatsHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli stats --help")
	}
	resp, err := doJSON(client, http.MethodGet, cfg.baseURL+"/v0/stats", nil, "", "")
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli stats --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli stats --help")
	}
	var out statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return reportError(cfg, exitError, "internal-error", "invalid response from API", "meshdns-cli stats --help")
	}
	if cfg.json {
		return writeSuccess(cfg, out)
	}
	renderStats(out)
	return exitOK
}

func cmdStatus(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printStatusHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli status --help")
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return reportError(cfg, exitInvalid, "invalid-input", "status requires a server name or id", "meshdns-cli status --help")
	}
	target := strings.TrimSpace(rest[0])
	server, err := lookupServer(client, cfg.baseURL, target)
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli status --help")
	}
	if cfg.json {
		return writeSuccess(cfg, server)
	}
	renderStatus(server)
	return exitOK
}

func cmdDelist(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printDelistHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("delist", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", cfg.force, "")
	writeKey := fs.String("write-key", cfg.writeKey, "")
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli delist --help")
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return reportError(cfg, exitInvalid, "invalid-input", "delist requires a server name or id", "meshdns-cli delist --help")
	}
	if strings.TrimSpace(*writeKey) == "" {
		return reportError(cfg, exitAuth, "unauthorized", "missing write key; set --write-key or MESHDNS_WRITE_KEY", "meshdns-cli delist --help")
	}
	target := strings.TrimSpace(rest[0])
	server, err := lookupServer(client, cfg.baseURL, target)
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli delist --help")
	}
	if !*force {
		if ok, reason := confirmDeletion(server.Name, server.ID); !ok {
			return reportError(cfg, exitInvalid, "invalid-input", reason, "meshdns-cli delist --help")
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, cfg.baseURL+"/v0/servers/"+url.PathEscape(server.ID), nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*writeKey))
	resp, err := client.Do(req)
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli delist --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return reportError(cfg, exitAuth, "unauthorized", "missing or invalid write key", "meshdns-cli delist --help")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli delist --help")
	}
	if cfg.json {
		return writeSuccess(cfg, map[string]any{"deleted": true, "server_id": server.ID})
	}
	fmt.Printf("Delisted %s (%s)\n", server.Name, server.ID)
	return exitOK
}

func cmdCapabilities(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printCapabilitiesHelp()
		return exitOK
	}
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(cfg.commandArgs); err != nil {
		return reportError(cfg, exitInvalid, "invalid-input", err.Error(), "meshdns-cli capabilities --help")
	}
	resp, err := doJSON(client, http.MethodGet, cfg.baseURL+"/v0/capabilities", nil, "", "")
	if err != nil {
		return classifyHTTPError(cfg, err, "meshdns-cli capabilities --help")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handleAPIError(cfg, resp, "meshdns-cli capabilities --help")
	}
	var out capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return reportError(cfg, exitError, "internal-error", "invalid response from API", "meshdns-cli capabilities --help")
	}
	if cfg.json {
		return writeSuccess(cfg, out)
	}
	renderCapabilities(out.Capabilities)
	return exitOK
}

func cmdDoctor(client *http.Client, cfg appConfig) exitCode {
	if helpRequested(cfg.commandArgs) {
		printDoctorHelp()
		return exitOK
	}
	resp, err := doJSON(client, http.MethodGet, cfg.baseURL+"/v0/stats", nil, "", "")
	if err != nil {
		return reportError(cfg, exitError, "unreachable", "API /v0/stats is unreachable", "meshdns-cli doctor")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return reportError(cfg, exitError, "unreachable", "API /v0/stats is unreachable", "meshdns-cli doctor")
	}
	if cfg.json {
		return writeSuccess(cfg, map[string]any{"healthy": true})
	}
	fmt.Println("healthy")
	return exitOK
}

func lookupServer(client *http.Client, baseURL, target string) (server, error) {
	resp, err := doJSON(client, http.MethodGet, baseURL+"/v0/servers?query="+url.QueryEscape(target), nil, "", "")
	if err != nil {
		return server{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return server{}, apiHTTPError(resp.StatusCode, resp)
	}
	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return server{}, err
	}
	if len(out.Servers) > 0 {
		first := out.Servers[0]
		if strings.EqualFold(first.ID, target) || strings.EqualFold(first.Name, target) {
			return first, nil
		}
	}
	if looksLikeID(target) {
		resp2, err := doJSON(client, http.MethodGet, baseURL+"/v0/servers/"+url.PathEscape(target), nil, "", "")
		if err != nil {
			return server{}, err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusNotFound {
			return server{}, apiHTTPError(resp2.StatusCode, resp2)
		}
		if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
			return server{}, apiHTTPError(resp2.StatusCode, resp2)
		}
		var got server
		if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
			return server{}, err
		}
		return got, nil
	}
	if len(out.Servers) == 0 {
		return server{}, apiHTTPError(http.StatusNotFound, nil)
	}
	return out.Servers[0], nil
}

func confirmDeletion(name, id string) (bool, string) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, "refusing to delete without --force or interactive confirmation"
	}
	fmt.Fprintf(os.Stderr, "Delete %s (%s)? Type yes to continue: ", name, id)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.EqualFold(strings.TrimSpace(line), "yes") {
		return true, ""
	}
	return false, "deletion cancelled"
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func renderList(servers []server) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tUP\tUPTIME")
	for _, s := range servers {
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", s.Name, s.Status, s.Up, formatPercent(s.Uptime30d))
	}
	_ = w.Flush()
}

func renderResolve(servers []server) {
	for i, s := range servers {
		fmt.Printf("%d. %s (%s, %s)\n", i+1, s.Name, s.Status, formatPercent(s.Uptime30d))
	}
}

func renderStats(s statsResponse) {
	fmt.Printf("servers active: %d\n", s.ServersActive)
	fmt.Printf("servers total: %d\n", s.ServersTotal)
	fmt.Printf("up count: %d\n", s.UpCount)
	fmt.Printf("resolutions 24h: %d\n", s.Resolutions24)
	fmt.Printf("probes 24h: %d\n", s.Probes24)
}

func renderStatus(s server) {
	fmt.Printf("name: %s\n", s.Name)
	fmt.Printf("id: %s\n", s.ID)
	fmt.Printf("status: %s\n", s.Status)
	fmt.Printf("up: %t\n", s.Up)
	fmt.Printf("uptime_30d: %s\n", formatPercent(s.Uptime30d))
	fmt.Printf("server_url: %s\n", s.ServerURL)
	fmt.Printf("health_url: %s\n", s.HealthURL)
	fmt.Printf("probe_method: %s\n", s.ProbeMethod)
	fmt.Printf("capabilities: %s\n", strings.Join(s.Capabilities, ", "))
	if s.Description != "" {
		fmt.Printf("description: %s\n", s.Description)
	}
	if s.LastChecked != "" {
		fmt.Printf("last_checked_at: %s\n", s.LastChecked)
	}
}

func renderCapabilities(caps []capabilityInfo) {
	sort.SliceStable(caps, func(i, j int) bool {
		if caps[i].ServerCount == caps[j].ServerCount {
			return caps[i].Name < caps[j].Name
		}
		return caps[i].ServerCount > caps[j].ServerCount
	})
	for _, c := range caps {
		fmt.Printf("%s\t%d\n", c.Name, c.ServerCount)
	}
}

func formatPercent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func printGlobalHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli [--url URL] [--json] <command> [flags]")
	fmt.Fprintln(os.Stdout, "commands: register, list, resolve, stats, status, delist, capabilities, doctor")
	fmt.Fprintln(os.Stdout, "global flags: --url, --json, --write-key, --force")
}

func printRegisterHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli register --name NAME --server-url URL --capabilities a,b [flags]")
	fmt.Fprintln(os.Stdout, "flags: --description, --health-url, --owner-contact, --probe-method")
}

func printListHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli list [--query Q] [--capability C] [--status S] [--limit N]")
	fmt.Fprintln(os.Stdout, "flags: --json")
}

func printResolveHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli resolve <capability>")
	fmt.Fprintln(os.Stdout, "flags: --json")
}

func printStatsHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli stats")
	fmt.Fprintln(os.Stdout, "flags: --json")
}

func printStatusHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli status <name-or-id>")
	fmt.Fprintln(os.Stdout, "flags: --json")
}

func printDelistHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli delist [--write-key KEY] [--force] <name-or-id>")
}

func printCapabilitiesHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli capabilities")
	fmt.Fprintln(os.Stdout, "flags: --json")
}

func printDoctorHelp() {
	fmt.Fprintln(os.Stdout, "meshdns-cli doctor")
}

func suggestedCommand(cmd string) string {
	if cmd == "" {
		return "meshdns-cli --help"
	}
	return "meshdns-cli " + cmd + " --help"
}

func writeSuccess(cfg appConfig, data interface{}) exitCode {
	if !cfg.json {
		return exitOK
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]interface{}{"ok": true, "data": data})
	return exitOK
}

func reportError(cfg appConfig, code exitCode, errCode, msg, suggested string) exitCode {
	if cfg.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]interface{}{
			"ok": false,
			"error": map[string]string{
				"code":              errCode,
				"message":           msg,
				"suggested_command": suggested,
			},
		})
	} else {
		if suggested != "" {
			fmt.Fprintln(os.Stderr, msg)
			fmt.Fprintln(os.Stderr, "suggested:", suggested)
		} else {
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	return code
}

func doJSON(client *http.Client, method, rawURL string, body []byte, authKey, contentType string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}
	return client.Do(req)
}

func classifyHTTPError(cfg appConfig, err error, suggested string) exitCode {
	return reportError(cfg, exitError, "network-error", err.Error(), suggested)
}

func handleAPIError(cfg appConfig, resp *http.Response, suggested string) exitCode {
	status := resp.StatusCode
	body, _ := io.ReadAll(resp.Body)
	code := "api-error"
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	if len(body) > 0 {
		var remote remoteError
		if json.Unmarshal(body, &remote) == nil && remote.Error.Code != "" {
			code = remote.Error.Code
			msg = fmt.Sprint(remote.Error.Detail)
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return reportError(cfg, exitAuth, code, msg, suggested)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return reportError(cfg, exitInvalid, code, msg, suggested)
	default:
		return reportError(cfg, exitError, code, msg, suggested)
	}
}

func apiHTTPError(status int, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("http %d", status)
	}
	return fmt.Errorf("http %d: %s", status, resp.Status)
}

func looksLikeID(v string) bool {
	if len(v) < 16 {
		return false
	}
	for _, r := range v {
		if r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
