package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Each Resource builder is constructed via Client.Events / Client.Claims /
// Client.Relationships / Client.Embeddings. Filter methods return the same
// builder so calls chain; terminal verbs (List, Append) take a context
// and return the typed result.
//
// The accessor names predate v0.85.0, which renamed the wire resources to the
// brain vocabulary (ADR 0011): Events hits /v1/episodes, Claims hits
// /v1/beliefs and Relationships hits /v1/associations. Every one of those
// endpoints requires a bearer token — reads included — unless the server was
// started with --public-reads; supply it with client.WithToken.
//
// Builders are short-lived and not safe for concurrent use across
// goroutines — construct a fresh one per request via the resource
// accessor.

// EventsBuilder is a fluent builder for the /v1/episodes endpoint.
// (The Go type keeps the pre-v0.85.0 "Events" name; the wire resource is
// episodes.)
type EventsBuilder struct {
	c      *Client
	limit  int
	offset int
	runID  string
}

// Limit sets the page size. Server caps at 200; values <= 0 use the
// server default of 50.
func (b *EventsBuilder) Limit(n int) *EventsBuilder { b.limit = n; return b }

// Offset sets the pagination offset.
func (b *EventsBuilder) Offset(n int) *EventsBuilder { b.offset = n; return b }

// RunID scopes the listing to episodes emitted under a single run_id — the
// server filters via ListByRunID rather than returning every episode, so a
// consumer that partitions memory by run (a per-tenant, per-session, or
// per-subject boundary) no longer has to page the whole store and filter
// client-side. Empty string clears the filter. Token whitelists still apply.
func (b *EventsBuilder) RunID(id string) *EventsBuilder { b.runID = id; return b }

// List hits GET /v1/episodes with the configured filters.
func (b *EventsBuilder) List(ctx context.Context) (*ListEventsResponse, error) {
	var out ListEventsResponse
	q := url.Values{}
	addPagination(q, b.limit, b.offset)
	if b.runID != "" {
		q.Set("run_id", b.runID)
	}
	if err := b.c.do(ctx, http.MethodGet, withQuery("/v1/episodes", q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Append hits POST /v1/episodes, sending the batch under the "episodes"
// wrapper key. Idempotent — episodes with IDs already in the registry are
// no-ops.
func (b *EventsBuilder) Append(ctx context.Context, events []Event) (*AppendResponse, error) {
	body := struct {
		Events []Event `json:"episodes"`
	}{Events: events}
	var out AppendResponse
	if err := b.c.do(ctx, http.MethodPost, "/v1/episodes", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClaimsBuilder is a fluent builder for the /v1/beliefs endpoint.
// (The Go type keeps the pre-v0.85.0 "Claims" name; the wire resource is
// beliefs.)
type ClaimsBuilder struct {
	c           *Client
	limit       int
	offset      int
	claimType   string
	claimStatus string
	runID       string
}

// Limit sets the page size.
func (b *ClaimsBuilder) Limit(n int) *ClaimsBuilder { b.limit = n; return b }

// Offset sets the pagination offset.
func (b *ClaimsBuilder) Offset(n int) *ClaimsBuilder { b.offset = n; return b }

// Type filters claims by claim type: fact, hypothesis, or decision.
// Empty string clears the filter.
func (b *ClaimsBuilder) Type(t string) *ClaimsBuilder { b.claimType = t; return b }

// Status filters claims by lifecycle status: active, contested, resolved,
// or deprecated. Empty string clears the filter.
func (b *ClaimsBuilder) Status(s string) *ClaimsBuilder { b.claimStatus = s; return b }

// RunID scopes the listing to beliefs whose evidence points at episodes emitted
// under a single run_id — the tenant boundary. The server resolves run_id →
// episodes → belief evidence, so a consumer partitioning memory by run gets
// only that partition's beliefs without paging every belief and mapping
// evidence client-side. An empty allowed-episode set (no episodes for the run)
// returns no beliefs rather than leaking the unfiltered set. Empty string
// clears the filter.
func (b *ClaimsBuilder) RunID(id string) *ClaimsBuilder { b.runID = id; return b }

// List hits GET /v1/beliefs with the configured filters. Evidence links
// for the returned beliefs are included in the response so the local
// query engine can resolve them back to episodes.
func (b *ClaimsBuilder) List(ctx context.Context) (*ListClaimsResponse, error) {
	var out ListClaimsResponse
	q := url.Values{}
	addPagination(q, b.limit, b.offset)
	if b.claimType != "" {
		q.Set("type", b.claimType)
	}
	if b.claimStatus != "" {
		q.Set("status", b.claimStatus)
	}
	if b.runID != "" {
		q.Set("run_id", b.runID)
	}
	if err := b.c.do(ctx, http.MethodGet, withQuery("/v1/beliefs", q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Append hits POST /v1/beliefs, sending the batch under the "beliefs" wrapper
// key. Pass evidence to insert the belief → episode link rows in the same
// request; the referenced episodes must already exist on the server.
func (b *ClaimsBuilder) Append(ctx context.Context, claims []Claim, evidence []EvidenceLink) (*AppendResponse, error) {
	body := AppendClaimsBody{Claims: claims, Evidence: evidence}
	var out AppendResponse
	if err := b.c.do(ctx, http.MethodPost, "/v1/beliefs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RelationshipsBuilder is a fluent builder for the /v1/associations endpoint.
// (The Go type keeps the pre-v0.85.0 "Relationships" name; the wire resource
// is associations.)
type RelationshipsBuilder struct {
	c       *Client
	limit   int
	offset  int
	relType string
}

// Limit sets the page size.
func (b *RelationshipsBuilder) Limit(n int) *RelationshipsBuilder { b.limit = n; return b }

// Offset sets the pagination offset.
func (b *RelationshipsBuilder) Offset(n int) *RelationshipsBuilder { b.offset = n; return b }

// Type filters relationships by type: supports or contradicts. Empty
// string clears the filter.
func (b *RelationshipsBuilder) Type(t string) *RelationshipsBuilder { b.relType = t; return b }

// List hits GET /v1/associations with the configured filters.
func (b *RelationshipsBuilder) List(ctx context.Context) (*ListRelationshipsResponse, error) {
	var out ListRelationshipsResponse
	q := url.Values{}
	addPagination(q, b.limit, b.offset)
	if b.relType != "" {
		q.Set("type", b.relType)
	}
	if err := b.c.do(ctx, http.MethodGet, withQuery("/v1/associations", q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Append hits POST /v1/associations, sending the batch under the
// "associations" wrapper key. The referenced from/to beliefs must already
// exist on the server.
func (b *RelationshipsBuilder) Append(ctx context.Context, rels []Relationship) (*AppendResponse, error) {
	body := struct {
		Relationships []Relationship `json:"associations"`
	}{Relationships: rels}
	var out AppendResponse
	if err := b.c.do(ctx, http.MethodPost, "/v1/associations", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EmbeddingsBuilder is a fluent builder for the /v1/embeddings endpoint.
type EmbeddingsBuilder struct {
	c          *Client
	limit      int
	offset     int
	entityType string
}

// Limit sets the page size.
func (b *EmbeddingsBuilder) Limit(n int) *EmbeddingsBuilder { b.limit = n; return b }

// Offset sets the pagination offset.
func (b *EmbeddingsBuilder) Offset(n int) *EmbeddingsBuilder { b.offset = n; return b }

// EntityType filters embeddings by their entity type: event or claim.
// Empty string clears the filter.
func (b *EmbeddingsBuilder) EntityType(t string) *EmbeddingsBuilder { b.entityType = t; return b }

// List hits GET /v1/embeddings with the configured filters. Vector
// values come back as JSON float arrays and round-trip bit-exact with
// the server's storage.
func (b *EmbeddingsBuilder) List(ctx context.Context) (*ListEmbeddingsResponse, error) {
	var out ListEmbeddingsResponse
	q := url.Values{}
	addPagination(q, b.limit, b.offset)
	if b.entityType != "" {
		q.Set("entity_type", b.entityType)
	}
	if err := b.c.do(ctx, http.MethodGet, withQuery("/v1/embeddings", q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Append hits POST /v1/embeddings.
func (b *EmbeddingsBuilder) Append(ctx context.Context, embs []Embedding) (*AppendResponse, error) {
	body := struct {
		Embeddings []Embedding `json:"embeddings"`
	}{Embeddings: embs}
	var out AppendResponse
	if err := b.c.do(ctx, http.MethodPost, "/v1/embeddings", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// addPagination writes limit/offset into q only when explicitly set
// (positive). Lets the server apply its own defaults.
func addPagination(q url.Values, limit, offset int) {
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
}

func withQuery(path string, q url.Values) string {
	encoded := q.Encode()
	if encoded == "" {
		return path
	}
	return path + "?" + encoded
}
