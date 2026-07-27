package trust

import (
	"math"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// test_decisiveness used to be added to the weighted sum UNCONDITIONALLY at a
// "neutral" 0.5 while being reported in the signals breakdown only for tests.
// For an additive term there is no neutral value, and the mismatch was visible
// two ways:
//
//   - `mnemos why-trust <fact>` listed contributions summing to score − 0.035,
//     because the term was in the score but not in the list;
//   - a non-test claim with every signal perfect capped at 0.965, not 1.0.
//
// The signal is now excluded for non-tests and the remaining five weights are
// renormalised.

const floatTol = 1e-9

func TestBuildReport_NonTestContributionsSumToScore(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	in := CredibilityInputs{
		CurrentTrust:    0.72,
		SourceAuthority: 0.64,
		CitationCount:   4,
		Liveness:        domain.LivenessLive,
		LastVerified:    now.Add(-48 * time.Hour),
		CreatedAt:       now.Add(-72 * time.Hour),
		Now:             now,
		IsTest:          false,
	}
	score, signals, _, _ := BuildReport(in)

	var sum float64
	for _, s := range signals {
		if s.Name == "test_decisiveness" {
			t.Fatalf("non-test claim must not report a test_decisiveness signal: %+v", s)
		}
		sum += s.Contribution
	}
	if math.Abs(sum-score) > floatTol {
		t.Fatalf("reported contributions sum to %.6f but score is %.6f (Δ%.6f) — why-trust cannot explain the score",
			sum, score, score-sum)
	}
}

func TestBuildReport_PerfectNonTestClaimReachesOne(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	in := CredibilityInputs{
		CurrentTrust:    1.0,
		SourceAuthority: 1.0,
		CitationCount:   1000, // saturates the log-scaled citation signal
		Liveness:        domain.LivenessLive,
		LastVerified:    now,
		ValidFrom:       now,
		CreatedAt:       now,
		Now:             now,
		IsTest:          false,
	}
	score, _, _, _ := BuildReport(in)
	if math.Abs(score-1.0) > 1e-6 {
		t.Fatalf("a perfect non-test claim should score 1.0, got %.6f (the old unconditional "+
			"0.5*wTest term capped it at 0.965)", score)
	}
}

func TestBuildReport_TestContributionsStillSumToScore(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	in := CredibilityInputs{
		CurrentTrust:    0.6,
		SourceAuthority: 0.7,
		CitationCount:   2,
		Liveness:        domain.LivenessLive,
		Now:             now,
		IsTest:          true,
		TestLastRunAt:   now.Add(-6 * time.Hour),
		TestPassCount:   9,
		TestFailCount:   1,
	}
	score, signals, _, _ := BuildReport(in)

	var sum float64
	sawTest := false
	for _, s := range signals {
		if s.Name == "test_decisiveness" {
			sawTest = true
		}
		sum += s.Contribution
	}
	if !sawTest {
		t.Fatalf("a test claim must report test_decisiveness; signals=%+v", signals)
	}
	if math.Abs(sum-score) > floatTol {
		t.Fatalf("test-claim contributions sum to %.6f but score is %.6f", sum, score)
	}
}

// TestSignalWeights_BothModesSumToOne pins the renormalisation itself: whichever
// signal set is in play, the additive weights are a proper convex combination,
// so the score stays a true 0–1 quantity rather than one with an unreachable top.
func TestSignalWeights_BothModesSumToOne(t *testing.T) {
	for _, isTest := range []bool{true, false} {
		wb, wa, wc, wr, wl := signalWeights(isTest)
		sum := wb + wa + wc + wr + wl
		if isTest {
			sum += wTest
		}
		if math.Abs(sum-1.0) > floatTol {
			t.Errorf("isTest=%v: additive weights sum to %.9f, want 1.0", isTest, sum)
		}
	}
}

// TestBuildReport_TestDecisivenessOnlyMovesTestScores is the mutation guard:
// with the term reintroduced unconditionally, changing pass/fail counts on a
// NON-test claim would move its score. It must not.
func TestBuildReport_TestDecisivenessOnlyMovesTestScores(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := CredibilityInputs{
		CurrentTrust: 0.5, SourceAuthority: 0.5, Liveness: domain.LivenessLive,
		CreatedAt: now.Add(-24 * time.Hour), Now: now, IsTest: false,
	}
	decisive := base
	decisive.TestPassCount, decisive.TestFailCount = 50, 0
	flaky := base
	flaky.TestPassCount, flaky.TestFailCount = 25, 25

	a, _ := ScoreCredibility(decisive)
	b, _ := ScoreCredibility(flaky)
	if math.Abs(a-b) > floatTol {
		t.Fatalf("pass/fail counts must not affect a non-test claim: decisive=%.6f flaky=%.6f", a, b)
	}

	// The same two inputs marked as tests must diverge, or the signal is dead.
	decisive.IsTest, flaky.IsTest = true, true
	ta, _ := ScoreCredibility(decisive)
	tb, _ := ScoreCredibility(flaky)
	if ta <= tb {
		t.Fatalf("a decisive test should outscore a flaky one: decisive=%.6f flaky=%.6f", ta, tb)
	}
}
