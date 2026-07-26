package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/govwrite"
	"go.klarlabs.de/mnemos/internal/store"
)

const (
	registryConfigName    = "config.json"
	pushBatchSize         = 100
	pullPageSize          = 100
	registryHTTPTimeout   = 60 * time.Second
	provenanceRegistryKey = "pulled_from_registry"
	provenancePulledAtKey = "pulled_at"
)

// registryConfig is the persisted shape of .mnemos/config.json. Only the
// registry block is populated today; the file is namespaced so future
// settings (preferred LLM, default scope, etc.) can sit alongside.
type registryConfig struct {
	Registry registrySettings `json:"registry"`
}

type registrySettings struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
}

// resolveRegistry returns the registry URL and token for the current project,
// merging (in order of precedence): CLI flags > env vars > project config.
// Returns an error only when no source provides a URL.
func resolveRegistry(flagURL, flagToken string) (string, string, error) {
	regURL := strings.TrimSpace(flagURL)
	token := flagToken

	if regURL == "" {
		regURL = strings.TrimSpace(os.Getenv("MNEMOS_REGISTRY_URL"))
	}
	if token == "" {
		token = os.Getenv("MNEMOS_REGISTRY_TOKEN")
	}

	if regURL == "" {
		if cfg, err := loadProjectConfig(); err == nil {
			regURL = strings.TrimSpace(cfg.Registry.URL)
			if token == "" {
				token = cfg.Registry.Token
			}
		}
	}

	if regURL == "" {
		return "", "", fmt.Errorf("no registry URL configured — set MNEMOS_REGISTRY_URL, pass --url, or run 'mnemos registry connect <url>'")
	}
	if _, err := url.Parse(regURL); err != nil {
		return "", "", fmt.Errorf("invalid registry URL %q: %w", regURL, err)
	}
	return strings.TrimRight(regURL, "/"), token, nil
}

func projectConfigPath() (string, error) {
	_, root, ok := findProjectDB()
	if !ok {
		return "", fmt.Errorf("no project (.mnemos/) found — run 'mnemos init' first")
	}
	return filepath.Join(root, ".mnemos", registryConfigName), nil
}

func loadProjectConfig() (registryConfig, error) {
	p, err := projectConfigPath()
	if err != nil {
		return registryConfig{}, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // G304: project config in user-owned .mnemos directory
	if err != nil {
		return registryConfig{}, err
	}
	var cfg registryConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return registryConfig{}, fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg, nil
}

func saveProjectConfig(cfg registryConfig) (string, error) {
	p, err := projectConfigPath()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}

// handleRegistry dispatches the `mnemos registry <subcommand>` family.
// Today only `connect` exists; later: `disconnect`, `status`, `set-token`.
func handleRegistry(args []string, _ Flags) {
	if len(args) == 0 {
		exitWithMnemosError(false, NewUserError("registry requires a subcommand: connect"))
		return
	}
	switch args[0] {
	case "connect":
		handleRegistryConnect(args[1:])
	default:
		exitWithMnemosError(false, NewUserError("unknown registry subcommand %q (want connect)", args[0]))
	}
}

func handleRegistryConnect(args []string) {
	if len(args) == 0 {
		exitWithMnemosError(false, NewUserError("registry connect requires a URL\n  mnemos registry connect <url> [--token <token>]"))
		return
	}
	regURL := args[0]
	args = args[1:]
	token := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--token" {
			if i+1 >= len(args) {
				exitWithMnemosError(false, NewUserError("--token requires a value"))
				return
			}
			token = args[i+1]
			i++
			continue
		}
		exitWithMnemosError(false, NewUserError("unknown flag %q", args[i]))
		return
	}
	if _, err := url.Parse(regURL); err != nil {
		exitWithMnemosError(false, NewUserError("invalid URL %q: %v", regURL, err))
		return
	}
	cfg := registryConfig{Registry: registrySettings{URL: strings.TrimRight(regURL, "/"), Token: token}}
	written, err := saveProjectConfig(cfg)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "save config"))
		return
	}
	fmt.Printf("connected to registry: %s\n", cfg.Registry.URL)
	fmt.Printf("config saved: %s\n", written)
	if token != "" {
		fmt.Println("auth: bearer token configured")
	} else {
		fmt.Println("auth: no token (registry must allow open writes)")
	}
}

// handlePush uploads all local events, claims, and relationships to the
// configured registry. Idempotent — registries upsert by ID, so re-running
// is safe. Reports counts per resource at the end.
func handlePush(args []string, _ Flags) {
	flagURL, flagToken := parseRegistryFlags(args)
	regURL, token, err := resolveRegistry(flagURL, flagToken)
	if err != nil {
		exitWithMnemosError(false, NewUserError("%s", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), registryHTTPTimeout*5)
	defer cancel()

	conn, err := openConn(ctx)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "open database"))
		return
	}
	defer closeConn(conn)

	client := &http.Client{Timeout: registryHTTPTimeout}

	events, err := loadAllEventsForPush(ctx, conn)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "load events"))
		return
	}
	claims, evidence, err := loadAllClaimsForPush(ctx, conn)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "load claims"))
		return
	}
	rels, err := loadAllRelationshipsForPush(ctx, conn)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "load relationships"))
		return
	}

	pushedEvents, err := pushBatched(ctx, client, regURL+"/v1/episodes", token, "episodes", batchToAny(eventsToBatches(events)))
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "push events"))
		return
	}
	pushedClaims, err := pushBatched(ctx, client, regURL+"/v1/beliefs", token, "beliefs", batchToAny(claimsToBatches(claims, evidence)))
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "push claims"))
		return
	}
	pushedRels, err := pushBatched(ctx, client, regURL+"/v1/associations", token, "associations", batchToAny(relsToBatches(rels)))
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "push relationships"))
		return
	}
	embeddings, err := loadAllEmbeddingsForPush(ctx, conn)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "load embeddings"))
		return
	}
	pushedEmbeddings, err := pushBatched(ctx, client, regURL+"/v1/embeddings", token, "embeddings", batchToAny(embeddingsToBatches(embeddings)))
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "push embeddings"))
		return
	}

	fmt.Printf("pushed to %s\n", regURL)
	fmt.Printf("  events:        %d\n", pushedEvents)
	fmt.Printf("  claims:        %d\n", pushedClaims)
	fmt.Printf("  relationships: %d\n", pushedRels)
	fmt.Printf("  embeddings:    %d\n", pushedEmbeddings)
}

// handlePull downloads all events, claims, and relationships from the
// configured registry into the local database. Uses pagination and respects
// the registry's max page size (caps at 200). Idempotent.
func handlePull(args []string, _ Flags) {
	flagURL, flagToken := parseRegistryFlags(args)
	regURL, token, err := resolveRegistry(flagURL, flagToken)
	if err != nil {
		exitWithMnemosError(false, NewUserError("%s", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), registryHTTPTimeout*5)
	defer cancel()

	gw, err := openWriter(ctx)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "open database"))
		return
	}
	defer closeWriter(gw)

	client := &http.Client{Timeout: registryHTTPTimeout}

	events, err := pullEvents(ctx, client, regURL, token)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "pull events"))
		return
	}
	stampPullProvenance(events, regURL, time.Now().UTC())
	claims, evidence, err := pullClaims(ctx, client, regURL, token)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "pull claims"))
		return
	}
	rels, err := pullRelationships(ctx, client, regURL, token)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "pull relationships"))
		return
	}

	insertedEvents, err := persistPulledEvents(ctx, gw, events)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "persist events"))
		return
	}
	insertedClaims, err := persistPulledClaims(ctx, gw, claims, evidence)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "persist claims"))
		return
	}
	insertedRels, err := persistPulledRelationships(ctx, gw, rels)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "persist relationships"))
		return
	}
	embeddings, err := pullEmbeddings(ctx, client, regURL, token)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "pull embeddings"))
		return
	}
	insertedEmbeddings, err := persistPulledEmbeddings(ctx, gw, embeddings)
	if err != nil {
		exitWithMnemosError(false, NewSystemError(err, "persist embeddings"))
		return
	}

	fmt.Printf("pulled from %s\n", regURL)
	fmt.Printf("  events:        %d\n", insertedEvents)
	fmt.Printf("  claims:        %d\n", insertedClaims)
	fmt.Printf("  relationships: %d\n", insertedRels)
	fmt.Printf("  embeddings:    %d\n", insertedEmbeddings)
}

func parseRegistryFlags(args []string) (string, string) {
	regURL := ""
	token := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--url":
			if i+1 < len(args) {
				regURL = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		default:
			exitWithMnemosError(false, NewUserError("unknown flag %q", args[i]))
		}
	}
	return regURL, token
}

func loadAllEventsForPush(ctx context.Context, conn *store.Conn) ([]eventDTO, error) {
	all, err := conn.Events.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]eventDTO, 0, len(all))
	for _, e := range all {
		out = append(out, eventDTO{
			ID:            e.ID,
			RunID:         e.RunID,
			SchemaVersion: e.SchemaVersion,
			Content:       e.Content,
			SourceInputID: e.SourceInputID,
			Timestamp:     e.Timestamp.UTC().Format(time.RFC3339),
			Metadata:      e.Metadata,
			IngestedAt:    e.IngestedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func loadAllClaimsForPush(ctx context.Context, conn *store.Conn) ([]claimDTO, []claimEvidenceItem, error) {
	allClaims, err := conn.Claims.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	claims := make([]claimDTO, 0, len(allClaims))
	for _, c := range allClaims {
		claims = append(claims, claimDTO{
			ID:         c.ID,
			Text:       c.Text,
			Type:       string(c.Type),
			Confidence: c.Confidence,
			Status:     string(c.Status),
			CreatedAt:  c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	allEvidence, err := conn.Claims.ListAllEvidence(ctx)
	if err != nil {
		return claims, nil, err
	}
	evidence := make([]claimEvidenceItem, 0, len(allEvidence))
	for _, ev := range allEvidence {
		evidence = append(evidence, claimEvidenceItem{ClaimID: ev.ClaimID, EventID: ev.EventID})
	}
	return claims, evidence, nil
}

func loadAllRelationshipsForPush(ctx context.Context, conn *store.Conn) ([]relationshipDTO, error) {
	all, err := conn.Relationships.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	rels := make([]relationshipDTO, 0, len(all))
	for _, r := range all {
		rels = append(rels, relationshipDTO{
			ID:          r.ID,
			Type:        string(r.Type),
			FromClaimID: r.FromClaimID,
			ToClaimID:   r.ToClaimID,
			CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return rels, nil
}

// eventsToBatches splits events into JSON-serializable request bodies of at
// most pushBatchSize each.
func eventsToBatches(events []eventDTO) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(events); i += pushBatchSize {
		end := i + pushBatchSize
		if end > len(events) {
			end = len(events)
		}
		out = append(out, map[string]any{"episodes": events[i:end]})
	}
	return out
}

func claimsToBatches(claims []claimDTO, evidence []claimEvidenceItem) []map[string]any {
	var out []map[string]any
	if len(claims) == 0 {
		return out
	}
	// Send the evidence list with the first batch so dependent FKs resolve
	// before the rest of the claims arrive. Server upserts evidence after
	// claims, so this works regardless of batch ordering, but consolidating
	// keeps the wire chatter low.
	for i := 0; i < len(claims); i += pushBatchSize {
		end := i + pushBatchSize
		if end > len(claims) {
			end = len(claims)
		}
		body := map[string]any{"beliefs": claims[i:end]}
		if i == 0 && len(evidence) > 0 {
			body["evidence"] = evidence
		}
		out = append(out, body)
	}
	return out
}

func relsToBatches(rels []relationshipDTO) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(rels); i += pushBatchSize {
		end := i + pushBatchSize
		if end > len(rels) {
			end = len(rels)
		}
		out = append(out, map[string]any{"associations": rels[i:end]})
	}
	return out
}

// batchToAny is just an explicit shim for type clarity in handlePush — the
// Go compiler accepts the cast implicitly, but spelling it out documents
// that pushBatched takes the same shape regardless of resource type.
func batchToAny(in []map[string]any) []map[string]any {
	return in
}

func pushBatched(ctx context.Context, client *http.Client, endpoint, token, resource string, batches []map[string]any) (int, error) {
	totalAccepted := 0
	for i, body := range batches {
		buf, err := json.Marshal(body)
		if err != nil {
			return totalAccepted, fmt.Errorf("encode %s batch %d: %w", resource, i, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf)) //nolint:gosec // G704: endpoint is operator-supplied via env/config, not user-supplied input
		if err != nil {
			return totalAccepted, fmt.Errorf("build %s request: %w", resource, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req) //nolint:gosec // G704: same operator-supplied endpoint as request
		if err != nil {
			return totalAccepted, fmt.Errorf("post %s batch %d: %w", resource, i, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return totalAccepted, fmt.Errorf("%s batch %d: server returned %d: %s", resource, i, resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		var ack appendResponse
		if err := json.Unmarshal(respBody, &ack); err == nil {
			totalAccepted += ack.Accepted
		}
	}
	return totalAccepted, nil
}

func pullEvents(ctx context.Context, client *http.Client, regURL, token string) ([]eventDTO, error) {
	var events []eventDTO
	offset := 0
	for {
		page, err := fetchPage(ctx, client, fmt.Sprintf("%s/v1/episodes?limit=%d&offset=%d", regURL, pullPageSize, offset), token)
		if err != nil {
			return nil, err
		}
		var body eventsResponse
		if err := json.Unmarshal(page, &body); err != nil {
			return nil, fmt.Errorf("decode events: %w", err)
		}
		events = append(events, body.Events...)
		if len(body.Events) == 0 || offset+len(body.Events) >= body.Total {
			break
		}
		offset += len(body.Events)
	}
	return events, nil
}

func pullClaims(ctx context.Context, client *http.Client, regURL, token string) ([]claimDTO, []claimEvidenceItem, error) {
	var claims []claimDTO
	var evidence []claimEvidenceItem
	offset := 0
	for {
		page, err := fetchPage(ctx, client, fmt.Sprintf("%s/v1/beliefs?limit=%d&offset=%d", regURL, pullPageSize, offset), token)
		if err != nil {
			return nil, nil, err
		}
		var body claimsResponse
		if err := json.Unmarshal(page, &body); err != nil {
			return nil, nil, fmt.Errorf("decode claims: %w", err)
		}
		claims = append(claims, body.Claims...)
		evidence = append(evidence, body.Evidence...)
		if len(body.Claims) == 0 || offset+len(body.Claims) >= body.Total {
			break
		}
		offset += len(body.Claims)
	}
	return claims, evidence, nil
}

func pullRelationships(ctx context.Context, client *http.Client, regURL, token string) ([]relationshipDTO, error) {
	var rels []relationshipDTO
	offset := 0
	for {
		page, err := fetchPage(ctx, client, fmt.Sprintf("%s/v1/associations?limit=%d&offset=%d", regURL, pullPageSize, offset), token)
		if err != nil {
			return nil, err
		}
		var body relationshipsResponse
		if err := json.Unmarshal(page, &body); err != nil {
			return nil, fmt.Errorf("decode relationships: %w", err)
		}
		rels = append(rels, body.Relationships...)
		if len(body.Relationships) == 0 || offset+len(body.Relationships) >= body.Total {
			break
		}
		offset += len(body.Relationships)
	}
	return rels, nil
}

func fetchPage(ctx context.Context, client *http.Client, endpoint, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil) //nolint:gosec // G704: endpoint is operator-supplied via env/config, not user-supplied input
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: same operator-supplied endpoint as request
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// persistPulledEvents persists events that don't already exist
// locally. Append on every backend is idempotent on (id), so we
// take a CountAll snapshot before and after to derive the
// "newly inserted" delta. Backend-agnostic.
func persistPulledEvents(ctx context.Context, gw *govwrite.Writer, events []eventDTO) (int, error) {
	conn := gw.Conn()
	before, err := conn.Events.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count events before pull: %w", err)
	}
	domEvents := make([]domain.Event, 0, len(events))
	for _, e := range events {
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			return 0, fmt.Errorf("parse event timestamp %s: %w", e.ID, err)
		}
		ingested, err := time.Parse(time.RFC3339, e.IngestedAt)
		if err != nil {
			return 0, fmt.Errorf("parse event ingested_at %s: %w", e.ID, err)
		}
		domEvents = append(domEvents, domain.Event{
			ID:            e.ID,
			RunID:         e.RunID,
			SchemaVersion: e.SchemaVersion,
			Content:       e.Content,
			SourceInputID: e.SourceInputID,
			Timestamp:     ts,
			Metadata:      e.Metadata,
			IngestedAt:    ingested,
		})
	}
	if _, err := gw.Events(ctx, domEvents); err != nil {
		return 0, fmt.Errorf("insert events: %w", err)
	}
	after, err := conn.Events.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count events after pull: %w", err)
	}
	return int(after - before), nil
}

// persistPulledClaims is the analogous backend-agnostic path for
// claims and their evidence links. As above, the "inserted" count
// is the CountAll delta — Upsert collapses duplicates silently.
func persistPulledClaims(ctx context.Context, gw *govwrite.Writer, claims []claimDTO, evidence []claimEvidenceItem) (int, error) {
	conn := gw.Conn()
	before, err := conn.Claims.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count claims before pull: %w", err)
	}
	domClaims := make([]domain.Claim, 0, len(claims))
	for _, c := range claims {
		ts, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("parse claim created_at %s: %w", c.ID, err)
		}
		domClaims = append(domClaims, domain.Claim{
			ID:         c.ID,
			Text:       c.Text,
			Type:       domain.ClaimType(c.Type),
			Confidence: c.Confidence,
			Status:     domain.ClaimStatus(c.Status),
			CreatedAt:  ts,
		})
	}
	// Attribute the pull. govwrite's ClaimReason doc names this path as one
	// that "relies on this" — and it passed the zero value, so a federated
	// claim landed with no status_history row at all. `mnemos audit who` and
	// the trust narrative then show no provenance for anything pulled: the
	// read succeeds, the trail is simply absent.
	if _, err := gw.Claims(ctx, domClaims, govwrite.ClaimReason{
		Reason:    "federation: registry pull",
		ChangedBy: "registry",
	}); err != nil {
		return 0, fmt.Errorf("upsert claims: %w", err)
	}
	links := make([]domain.ClaimEvidence, 0, len(evidence))
	for _, e := range evidence {
		links = append(links, domain.ClaimEvidence{ClaimID: e.ClaimID, EventID: e.EventID})
	}
	if _, err := gw.EvidenceLinks(ctx, links); err != nil {
		return 0, fmt.Errorf("upsert claim evidence: %w", err)
	}
	after, err := conn.Claims.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count claims after pull: %w", err)
	}
	return int(after - before), nil
}

// stampPullProvenance mutates each pulled event so its metadata records
// where it came from and when. The query engine surfaces these to the user
// at answer time so claims from a registry are distinguishable from local
// ones — the federation contract is "show me where each fact came from."
//
// Provenance is only added if not already set, so an event that was first
// pulled from registry A and then re-pulled from registry B keeps its
// original origin (consistent with first-write-wins on event id).
func stampPullProvenance(events []eventDTO, regURL string, at time.Time) {
	stamp := at.Format(time.RFC3339)
	for i := range events {
		if events[i].Metadata == nil {
			events[i].Metadata = map[string]string{}
		}
		if _, exists := events[i].Metadata[provenanceRegistryKey]; !exists {
			events[i].Metadata[provenanceRegistryKey] = regURL
			events[i].Metadata[provenancePulledAtKey] = stamp
		}
	}
}

func loadAllEmbeddingsForPush(ctx context.Context, conn *store.Conn) ([]embeddingDTO, error) {
	all, err := conn.Embeddings.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]embeddingDTO, 0, len(all))
	for _, e := range all {
		out = append(out, embeddingDTO{
			EntityID:   e.EntityID,
			EntityType: e.EntityType,
			Vector:     e.Vector,
			Model:      e.Model,
			Dimensions: e.Dimensions,
		})
	}
	return out, nil
}

func embeddingsToBatches(records []embeddingDTO) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(records); i += pushBatchSize {
		end := i + pushBatchSize
		if end > len(records) {
			end = len(records)
		}
		out = append(out, map[string]any{"embeddings": records[i:end]})
	}
	return out
}

func pullEmbeddings(ctx context.Context, client *http.Client, regURL, token string) ([]embeddingDTO, error) {
	var out []embeddingDTO
	offset := 0
	for {
		page, err := fetchPage(ctx, client, fmt.Sprintf("%s/v1/embeddings?limit=%d&offset=%d", regURL, pullPageSize, offset), token)
		if err != nil {
			return nil, err
		}
		var body embeddingsResponse
		if err := json.Unmarshal(page, &body); err != nil {
			return nil, fmt.Errorf("decode embeddings: %w", err)
		}
		out = append(out, body.Embeddings...)
		if len(body.Embeddings) == 0 || offset+len(body.Embeddings) >= body.Total {
			break
		}
		offset += len(body.Embeddings)
	}
	return out, nil
}

// persistPulledEmbeddings upserts pulled embeddings via the port
// repo. Embeddings are derived data — last write wins is the right
// semantic, so the count is just len(records).
func persistPulledEmbeddings(ctx context.Context, gw *govwrite.Writer, records []embeddingDTO) (int, error) {
	for _, e := range records {
		if err := gw.Embedding(ctx, e.EntityID, e.EntityType, e.Vector, e.Model, ""); err != nil {
			return 0, fmt.Errorf("upsert embedding %s/%s: %w", e.EntityID, e.EntityType, err)
		}
	}
	return len(records), nil
}

func persistPulledRelationships(ctx context.Context, gw *govwrite.Writer, rels []relationshipDTO) (int, error) {
	conn := gw.Conn()
	before, err := conn.Relationships.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count relationships before pull: %w", err)
	}
	domRels := make([]domain.Relationship, 0, len(rels))
	for _, r := range rels {
		ts, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("parse relationship created_at %s: %w", r.ID, err)
		}
		domRels = append(domRels, domain.Relationship{
			ID:          r.ID,
			Type:        domain.RelationshipType(r.Type),
			FromClaimID: r.FromClaimID,
			ToClaimID:   r.ToClaimID,
			CreatedAt:   ts,
		})
	}
	if _, err := gw.Relationships(ctx, domRels); err != nil {
		return 0, fmt.Errorf("upsert relationships: %w", err)
	}
	after, err := conn.Relationships.CountAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("count relationships after pull: %w", err)
	}
	return int(after - before), nil
}
