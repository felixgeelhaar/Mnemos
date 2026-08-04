package pipeline

import (
	"go.klarlabs.de/mnemos/internal/domain"
)

// Event metadata keys carrying where an ingestion came from.
//
// internal/ingest builds `source`/`source_path` on the Input, and
// parser.Normalizer copies every input key onto the event with an `input_`
// prefix (normalizer.go). These constants name the result so the pipeline is
// not matching hand-written strings against another package's convention.
const (
	sourceKindKey = "input_source"
	sourcePathKey = "input_source_path"
)

// StampSourceProvenance fills SourceDocument and SourceType on claims from the
// events they were extracted from.
//
// Both fields have been on domain.Belief since the epistemic-provenance work
// and were empty on every belief of a real 68,670-belief brain, because no
// ingestion path ever set them — the information existed on the event
// (`input_source_path` records the absolute file path) and simply stopped
// there. `trust.SourceAuthority` and the liveness signals are built on these,
// so the whole provenance dimension sat inert.
//
// Two rules keep it honest:
//
//   - AN EXISTING VALUE IS NEVER OVERWRITTEN. A caller that knows better than
//     the ingest metadata — markdown import, a direct API write — has already
//     said so, and re-deriving over it would be the #334 regression in a new
//     column.
//   - WHEN THE KIND IS UNKNOWN, NOTHING IS WRITTEN. Only a `file` ingestion and
//     a session capture carry enough to classify. `raw_text` alone could be a
//     transcript, a paste, or an API call, and guessing would put a confident
//     wrong value where an honest empty belongs — the same discipline the
//     volatility classifier follows in only ever acting on a confident signal.
func StampSourceProvenance(events []domain.Event, claims []domain.Claim, links []domain.ClaimEvidence) {
	if len(events) == 0 || len(claims) == 0 || len(links) == 0 {
		return
	}
	byEvent := make(map[string]domain.Event, len(events))
	for _, e := range events {
		byEvent[e.ID] = e
	}
	// First non-empty source wins, matching how scopeOfClaims resolves session
	// and workspace: evidence for one claim comes from one ingestion, so the
	// events agree in practice.
	docOf := make(map[string]string)
	typeOf := make(map[string]domain.SourceType)
	for _, l := range links {
		e, ok := byEvent[l.EventID]
		if !ok || e.Metadata == nil {
			continue
		}
		switch e.Metadata[sourceKindKey] {
		case "file":
			if _, seen := typeOf[l.ClaimID]; !seen {
				typeOf[l.ClaimID] = domain.SourceTypeDocument
				docOf[l.ClaimID] = e.Metadata[sourcePathKey]
			}
		default:
			// A capture carries a session id; that is a transcript whatever the
			// input format says. It has no stable document path — the transcript
			// is ephemeral — so only the type is recorded.
			if e.Metadata[SessionMetadataKey] != "" {
				if _, seen := typeOf[l.ClaimID]; !seen {
					typeOf[l.ClaimID] = domain.SourceTypeTranscript
				}
			}
		}
	}

	for i := range claims {
		if t, ok := typeOf[claims[i].ID]; ok && claims[i].SourceType == "" {
			claims[i].SourceType = t
		}
		if d := docOf[claims[i].ID]; d != "" && claims[i].SourceDocument == "" {
			claims[i].SourceDocument = d
		}
	}
}
