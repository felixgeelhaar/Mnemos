package mnemos

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/ports"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
	"go.klarlabs.de/mnemos/internal/trust"
)

// decayMem opens an isolated in-memory brain. Each test gets its own namespace
// so one test's beliefs never age into another's vitals.
func decayMem(t *testing.T, namespace string) *memory {
	t.Helper()
	for _, k := range []string{"MNEMOS_STORAGE", "MNEMOS_MODE", "MNEMOS_LLM_PROVIDER", "MNEMOS_API_KEY"} {
		t.Setenv(k, "")
	}
	mem, err := New(WithStorage("memory://"+namespace), WithPassiveMode(), WithActor("tester"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return mem.(*memory)
}

// decaySeq keeps seeded event ids unique across every belief a test writes.
var decaySeq int

// rememberAged writes a belief through the real write path — a real event at a
// real past instant, a real claim linked to it as evidence — and returns its id.
//
// Confidence is 0.8 rather than the ClaimItem default of 1.0 deliberately: at
// confidence 1.0 the freshness floor (0.3) lands exactly ON the low-trust floor
// (0.30), so a fully-confident belief mathematically never decays through it.
// 0.8 is the order of magnitude extraction actually assigns.
func rememberAged(t *testing.T, m *memory, text string, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Add(-age)
	decaySeq++
	evID := fmt.Sprintf("ev-decay-%d", decaySeq)
	if err := m.RememberEvent(ctx, Event{ID: evID, At: at, Type: "observation", Content: text}); err != nil {
		t.Fatalf("RememberEvent: %v", err)
	}
	id, err := m.RememberClaim(ctx, ClaimItem{
		Text: text, Confidence: 0.8, EventIDs: []string{evID}, ValidFrom: at,
	})
	if err != nil {
		t.Fatalf("RememberClaim: %v", err)
	}
	return id
}

// rescoreTrust runs the same trust recomputation production runs (pipeline's
// defaultTrustScorer / Consolidate's renormalisation pass) so the trust scores
// the vital reads are the store's own, derived from the stored evidence — not
// numbers a test wrote by hand.
func rescoreTrust(t *testing.T, m *memory) {
	t.Helper()
	scorer, ok := m.conn.Claims.(ports.TrustScorer)
	if !ok {
		t.Fatal("store does not score trust; the vital under test would be meaningless")
	}
	now := time.Now().UTC()
	if _, err := scorer.RecomputeTrust(context.Background(), func(confidence float64, evidenceCount int, latestEvidence time.Time) float64 {
		return trust.Score(confidence, evidenceCount, latestEvidence, now)
	}); err != nil {
		t.Fatalf("recompute trust: %v", err)
	}
}

func trustDecay(t *testing.T, m *memory) Vital {
	t.Helper()
	h, err := m.BrainHealth(context.Background())
	if err != nil {
		t.Fatalf("BrainHealth: %v", err)
	}
	v := vitalByName(h, "trust_decay")
	if v.Name == "MISSING" {
		t.Fatal("trust_decay vital is not reported at all")
	}
	return v
}

// A brain whose beliefs were all just written is not losing trust: nothing sits
// close enough to the floor to fall through it inside the horizon.
func TestTrustDecay_FreshBrainIsNotDecaying(t *testing.T) {
	m := decayMem(t, "decay-fresh")
	for i := 0; i < 5; i++ {
		rememberAged(t, m, fmt.Sprintf("the ledger reconciles nightly in region %d", i), 5*24*time.Hour)
	}
	rescoreTrust(t, m)

	v := trustDecay(t, m)
	if v.Value != 0 {
		t.Errorf("trust_decay over freshly-written beliefs = %v, want 0 (%s)", v.Value, v.Detail)
	}
	if v.Status != HealthOK {
		t.Errorf("status = %q, want %q", v.Status, HealthOK)
	}
}

// The vital is a RATE, and it has to move with the data rather than flip. Two
// beliefs about to fall through the floor is twice the outflow of one, and the
// second crosses the critical line the first only warns at.
func TestTrustDecay_TracksTheShareAboutToFallThroughTheFloor(t *testing.T) {
	m := decayMem(t, "decay-rate")
	// 65 days old at the default half-life: still above the trust floor today,
	// below it a horizon from now. Neither figure is asserted here — both come
	// out of the store's own trust recomputation over the stored evidence.
	const aging = 65 * 24 * time.Hour
	const fresh = 3 * 24 * time.Hour
	for i := 0; i < 8; i++ {
		rememberAged(t, m, fmt.Sprintf("the ledger reconciles nightly in region %d", i), fresh)
	}
	rememberAged(t, m, "the reconciliation window closes at midnight UTC", aging)
	rescoreTrust(t, m)

	one := trustDecay(t, m)
	if one.Value <= 0 {
		t.Fatalf("one aging belief in nine should register: got %v (%s)", one.Value, one.Detail)
	}
	if one.Status != HealthDegraded {
		t.Errorf("one in nine = %v should warn, got %q (%s)", one.Value, one.Status, one.Detail)
	}

	rememberAged(t, m, "the settlement batch starts after the window shuts", aging)
	rescoreTrust(t, m)
	two := trustDecay(t, m)
	if !(two.Value > one.Value) {
		t.Fatalf("a second decaying belief must raise the rate: %v then %v", one.Value, two.Value)
	}
	if two.Status != HealthUnhealthy {
		t.Errorf("two in ten = %v should be critical, got %q (%s)", two.Value, two.Status, two.Detail)
	}
}

// The point of the vital: it reads each belief's OWN half-life. Two beliefs of
// identical age, identical evidence and identical confidence decay at different
// rates once one is declared volatile — and only that one is flagged. The
// fixed-horizon staleness count cannot tell them apart, which is exactly why
// this is a separate vital and not a second reading of that one.
func TestTrustDecay_HonoursPerClaimHalfLife(t *testing.T) {
	m := decayMem(t, "decay-halflife")
	ctx := context.Background()
	const age = 5 * 24 * time.Hour
	at := time.Now().UTC().Add(-age)

	volatile := rememberAged(t, m, "the export worker runs on the batch queue", age)
	durable := rememberAged(t, m, "the export format was chosen for downstream parsers", age)
	// The same path `mnemos verify --half-life` takes: re-verify at the same
	// instant, but declare different decay profiles.
	if err := m.conn.Claims.MarkVerified(ctx, volatile, at, 14); err != nil {
		t.Fatalf("MarkVerified volatile: %v", err)
	}
	if err := m.conn.Claims.MarkVerified(ctx, durable, at, 365); err != nil {
		t.Fatalf("MarkVerified durable: %v", err)
	}
	rescoreTrust(t, m)

	// Precondition: the two differ ONLY in declared half-life. Their stored
	// trust scores are equal, so anything the vital reports about one and not
	// the other can only come from the decay model.
	claims, err := m.conn.Claims.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]float64{}
	for _, c := range claims {
		byID[c.ID] = c.TrustScore
	}
	if !approx(byID[volatile], byID[durable], 1e-6) {
		t.Fatalf("precondition: stored trust must be equal, got %v and %v", byID[volatile], byID[durable])
	}

	v := trustDecay(t, m)
	if v.Value != 0.5 {
		t.Errorf("trust_decay = %v, want 0.5 — only the volatile belief decays through the floor (%s)", v.Value, v.Detail)
	}
	if v.Status != HealthUnhealthy {
		t.Errorf("half the trusted corpus decaying within the horizon = %q, want %q", v.Status, HealthUnhealthy)
	}
}

// #325's standard: a vital with nothing behind it reports unknown, not a
// reassuring zero. An empty brain has no belief whose decay could be measured.
func TestTrustDecay_EmptyBrainIsUnknown(t *testing.T) {
	m := decayMem(t, "decay-empty")
	v := trustDecay(t, m)
	if v.Status != HealthUnknown {
		t.Errorf("trust_decay on an empty brain = %q, want %q", v.Status, HealthUnknown)
	}
	h, err := m.BrainHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != HealthOK {
		t.Errorf("an unmeasured vital must not drag the verdict to %q", h.Status)
	}
}

// A brain whose beliefs have ALREADY fallen below the floor has no trust left
// to lose there — reporting a healthy 0 would read as "nothing is decaying"
// when the truth is "everything already has". low_trust owns that state, and it
// must be the vital raising the alarm.
func TestTrustDecay_AlreadyDecayedBrainIsUnknownAndLowTrustCatchesIt(t *testing.T) {
	m := decayMem(t, "decay-spent")
	for i := 0; i < 4; i++ {
		rememberAged(t, m, fmt.Sprintf("the importer accepted fixed-width files in region %d", i), 3*365*24*time.Hour)
	}
	rescoreTrust(t, m)

	h, err := m.BrainHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v := vitalByName(h, "trust_decay"); v.Status != HealthUnknown {
		t.Errorf("trust_decay with no belief left above the floor = %q, want %q (%s)", v.Status, HealthUnknown, v.Detail)
	}
	if lt := vitalByName(h, "low_trust"); lt.Value == 0 || lt.Status == HealthOK {
		t.Errorf("low_trust must own the already-decayed state, got %+v", lt)
	}
}
