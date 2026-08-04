package pipeline_test

import (
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/pipeline"
)

func provFixture(meta map[string]string) ([]domain.Event, []domain.Claim, []domain.ClaimEvidence) {
	at := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ev := domain.Event{
		ID: "ev-1", RunID: "r", SchemaVersion: "1", Content: "x",
		SourceInputID: "in-1", Timestamp: at, IngestedAt: at, Metadata: meta,
	}
	c := domain.Claim{ID: "c-1", Text: "x", Type: domain.ClaimTypeFact, Confidence: 0.9,
		Status: domain.ClaimStatusActive, CreatedAt: at}
	return []domain.Event{ev}, []domain.Claim{c}, []domain.ClaimEvidence{{ClaimID: "c-1", EventID: "ev-1"}}
}

// The path was on the event the whole time and never reached the belief.
func TestStampSourceProvenance_CarriesTheFilePathOntoTheBelief(t *testing.T) {
	evs, claims, links := provFixture(map[string]string{
		"input_source":      "file",
		"input_source_path": "/repo/docs/runbook.md",
	})
	pipeline.StampSourceProvenance(evs, claims, links)

	if claims[0].SourceDocument != "/repo/docs/runbook.md" {
		t.Errorf("source_document = %q, want the ingested path", claims[0].SourceDocument)
	}
	if claims[0].SourceType != domain.SourceTypeDocument {
		t.Errorf("source_type = %q, want %q", claims[0].SourceType, domain.SourceTypeDocument)
	}
}

// A capture is a transcript whatever its input format says, and has no stable
// document path — the transcript is ephemeral, so only the type is recorded.
func TestStampSourceProvenance_CaptureIsATranscriptWithNoDocument(t *testing.T) {
	evs, claims, links := provFixture(map[string]string{
		"input_source":              "raw_text",
		pipeline.SessionMetadataKey: "sess-1",
	})
	pipeline.StampSourceProvenance(evs, claims, links)

	if claims[0].SourceType != domain.SourceTypeTranscript {
		t.Errorf("source_type = %q, want %q", claims[0].SourceType, domain.SourceTypeTranscript)
	}
	if claims[0].SourceDocument != "" {
		t.Errorf("source_document = %q, want empty — a transcript has no stable path",
			claims[0].SourceDocument)
	}
}

// When the kind is unknown, nothing is written. A confident wrong value is
// worse than an honest empty: `raw_text` alone could be a transcript, a paste
// or an API call, and downstream trust reads these fields.
func TestStampSourceProvenance_UnknownSourceWritesNothing(t *testing.T) {
	evs, claims, links := provFixture(map[string]string{"input_source": "raw_text"})
	pipeline.StampSourceProvenance(evs, claims, links)

	if claims[0].SourceType != "" || claims[0].SourceDocument != "" {
		t.Errorf("guessed provenance for an unclassifiable source: type=%q doc=%q",
			claims[0].SourceType, claims[0].SourceDocument)
	}
}

// An explicit value survives. A caller that already knows its provenance —
// markdown import, a direct API write — has said so, and re-deriving over it
// would be the #334 regression in a new column.
func TestStampSourceProvenance_NeverOverwritesAnExplicitValue(t *testing.T) {
	evs, claims, links := provFixture(map[string]string{
		"input_source":      "file",
		"input_source_path": "/repo/docs/runbook.md",
	})
	claims[0].SourceDocument = "https://wiki.internal/the-real-source"
	claims[0].SourceType = domain.SourceTypeWebPage

	pipeline.StampSourceProvenance(evs, claims, links)

	if claims[0].SourceDocument != "https://wiki.internal/the-real-source" {
		t.Errorf("source_document overwritten to %q", claims[0].SourceDocument)
	}
	if claims[0].SourceType != domain.SourceTypeWebPage {
		t.Errorf("source_type overwritten to %q", claims[0].SourceType)
	}
}

// No links, no events, or no metadata must all be safe no-ops: most ingestion
// paths carry none of this.
func TestStampSourceProvenance_IsSafeWithoutProvenance(t *testing.T) {
	evs, claims, links := provFixture(nil)
	pipeline.StampSourceProvenance(evs, claims, links)
	if claims[0].SourceType != "" || claims[0].SourceDocument != "" {
		t.Error("wrote provenance from an event carrying none")
	}
	pipeline.StampSourceProvenance(nil, claims, links)
	pipeline.StampSourceProvenance(evs, claims, nil)
}
