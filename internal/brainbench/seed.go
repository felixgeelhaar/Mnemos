package brainbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/extract"
	"go.klarlabs.de/mnemos/internal/pipeline"
	"go.klarlabs.de/mnemos/internal/relate"
	"go.klarlabs.de/mnemos/internal/store"
)

// SeedStats records what seeding produced, so the report can show the shape of
// the brain both arms started from.
type SeedStats struct {
	Events        int `json:"events"`
	Claims        int `json:"claims"`
	Relationships int `json:"relationships"`
	Embeddings    int `json:"embeddings"`
	// DedupedAtIngest counts claims the ingest-time exact-text deduper
	// collapsed BEFORE consolidation ever saw them. It matters because that
	// redundancy is not available for consolidation to take credit for: the
	// harness measures what consolidation adds ON TOP of ingest dedupe, which
	// is the only fair comparison.
	DedupedAtIngest int `json:"deduped_at_ingest"`
}

// Seed builds a brain at dsn from the scenario corpus.
//
// It reproduces the production ingest path (`mnemos process --embed`) offline
// and deterministically: rule-based extraction, ingest-time exact-text dedupe
// against what is already stored, within-batch relationship detection plus
// incremental detection against the existing store, then event and claim
// embeddings. Documents are ingested one at a time, in order, because that is
// how a brain actually accumulates — one capture session at a time — and it is
// the only way cross-session near-duplicates arise. Ingesting the corpus as a
// single batch would hand the ingest deduper the redundancy that consolidation
// is meant to be evaluated on.
//
// No LLM and no network: extraction is rule-based and embeddings come from the
// deterministic stub. The report says so; results here say nothing about the
// LLM extraction path.
func Seed(ctx context.Context, dsn string, sc Scenario) (SeedStats, error) {
	conn, err := store.Open(ctx, dsn)
	if err != nil {
		return SeedStats{}, fmt.Errorf("brainbench: open seed store: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if conn.Embeddings == nil {
		return SeedStats{}, fmt.Errorf("brainbench: store %q has no embeddings repository; "+
			"the dedupe stage of consolidation would be unmeasurable", dsn)
	}

	engine := extract.NewEngine()
	relator := relate.NewEngine()

	// A single wall-clock anchor for the whole seed. Deriving each document's
	// timestamp from a per-document time.Now() would make two arms seeded in
	// separate runs differ by milliseconds for no reason; anchoring once keeps
	// the seed a pure function of the scenario plus one instant.
	now := time.Now().UTC()

	var stats SeedStats
	for _, doc := range sc.Corpus {
		ts := now.AddDate(0, 0, -doc.AgeDays)
		// domain.Event requires a source input id. Defaulting it to the doc id
		// keeps `source` an optional, human-facing label in scenario YAML
		// instead of a mandatory field every author must remember to repeat.
		source := doc.Source
		if source == "" {
			source = doc.ID
		}
		ev := domain.Event{
			ID:            deterministicEventID(sc.ID, doc.ID),
			RunID:         sc.ID,
			SchemaVersion: "1.0",
			Content:       doc.Text,
			SourceInputID: source,
			Timestamp:     ts,
			// IngestedAt is the transaction time and is genuinely "now" even
			// for a back-dated document — back-dating models a fact observed
			// long ago, not a row written long ago.
			IngestedAt: now,
		}

		claims, links, err := engine.Extract(ctx, []domain.Event{ev})
		if err != nil {
			return stats, fmt.Errorf("brainbench: extract %s/%s: %w", sc.ID, doc.ID, err)
		}

		existing, err := conn.Claims.ListAll(ctx)
		if err != nil {
			return stats, fmt.Errorf("brainbench: list existing claims: %w", err)
		}

		before := len(claims)
		claims, links = pipeline.DedupeAgainstExisting(claims, links, existing)
		stats.DedupedAtIngest += before - len(claims)

		rels, err := relator.Detect(claims)
		if err != nil {
			return stats, fmt.Errorf("brainbench: relate %s/%s: %w", sc.ID, doc.ID, err)
		}
		if len(existing) > 0 && len(claims) > 0 {
			inc, err := relator.DetectIncremental(claims, existing)
			if err != nil {
				return stats, fmt.Errorf("brainbench: incremental relate %s/%s: %w", sc.ID, doc.ID, err)
			}
			rels = append(rels, inc...)
		}

		// Stamp a single attributed actor so nothing in the seeded brain is
		// attributed to the ambient OS user — that would make the seed depend
		// on who ran it.
		for i := range claims {
			claims[i].CreatedBy = SeedActor
		}
		for i := range rels {
			rels[i].CreatedBy = SeedActor
		}
		ev.CreatedBy = SeedActor

		if err := pipeline.PersistArtifacts(ctx, conn, []domain.Event{ev}, claims, links, rels); err != nil {
			return stats, fmt.Errorf("brainbench: persist %s/%s: %w", sc.ID, doc.ID, err)
		}

		if err := conn.Embeddings.Upsert(ctx, ev.ID, "event", EmbedText(ev.Content), StubEmbedderModel, ""); err != nil {
			return stats, fmt.Errorf("brainbench: embed event %s: %w", ev.ID, err)
		}
		stats.Embeddings++
		for _, c := range claims {
			if err := conn.Embeddings.Upsert(ctx, c.ID, "claim", EmbedText(c.Text), StubEmbedderModel, ""); err != nil {
				return stats, fmt.Errorf("brainbench: embed claim %s: %w", c.ID, err)
			}
			stats.Embeddings++
		}

		stats.Events++
		stats.Claims += len(claims)
		stats.Relationships += len(rels)
	}
	return stats, nil
}

// SeedActor attributes every seeded write, so a seed does not vary with the OS
// user running the harness.
const SeedActor = "brainbench"

// deterministicEventID derives a stable event id from the scenario and document
// ids. Stable ids keep a re-seeded brain comparable to an earlier one; a random
// or timestamp-derived id would make every run's stored rows differ for reasons
// unrelated to the experiment.
func deterministicEventID(scenarioID, docID string) string {
	sum := sha256.Sum256([]byte(scenarioID + "\x00" + docID))
	return "ev_bb_" + hex.EncodeToString(sum[:8])
}
