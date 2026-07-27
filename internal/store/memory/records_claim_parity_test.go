package memory

import (
	"reflect"
	"testing"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
)

// enumFieldValues pins a valid domain value for every domain.Claim field whose
// type is a constrained enum (or otherwise cannot take a made-up string). Every
// other field is filled generically by fillClaim. A new enum field that lands
// without an entry here is caught by fillClaim, which fails on any field it
// leaves at its zero value.
var enumFieldValues = map[string]any{
	"Type":         domain.ClaimTypeTestResult,
	"Status":       domain.ClaimStatusActive,
	"Lifecycle":    domain.ClaimLifecyclePromoted,
	"SubjectClass": domain.SubjectClassClass,
	"Durability":   domain.DurabilityDurable,
	"SourceType":   domain.SourceTypeGitCommit,
	"Liveness":     domain.LivenessLive,
	// Visibility is normalised on write, so an arbitrary string would round-trip
	// to "team" and mask a dropped field. Personal is the value the drop
	// actually corrupted in production.
	"Visibility": domain.VisibilityPersonal,
	"Scope":      domain.Scope{Service: "billing", Env: "prod", Team: "core"},
	"ConfidenceComponents": map[string]float64{
		"data_quality":  0.91,
		"corroboration": 0.42,
	},
}

// fillClaim sets EVERY field of a domain.Claim to a distinctive non-zero value.
// It walks the struct reflectively rather than listing fields, so a field added
// to domain.Claim is filled automatically — and if its kind is one the filler
// does not understand it stays zero and the test fails loudly instead of
// quietly checking nothing.
func fillClaim(t *testing.T) domain.Claim {
	t.Helper()
	var c domain.Claim
	v := reflect.ValueOf(&c).Elem()
	typ := v.Type()
	base := time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)

	for i := range typ.NumField() {
		field := typ.Field(i)
		fv := v.Field(i)
		if !fv.CanSet() {
			t.Fatalf("domain.Claim.%s is unexported; the memory backend cannot persist it", field.Name)
		}
		if pinned, ok := enumFieldValues[field.Name]; ok {
			pv := reflect.ValueOf(pinned)
			if !pv.Type().AssignableTo(field.Type) {
				t.Fatalf("enumFieldValues[%q] is %s, not assignable to %s", field.Name, pv.Type(), field.Type)
			}
			fv.Set(pv)
			continue
		}
		switch {
		case field.Type == reflect.TypeOf(time.Time{}):
			// Distinct instants per field so a cross-wired assignment
			// (LastExecuted written from TestLastRunAt, say) also fails.
			fv.Set(reflect.ValueOf(base.Add(time.Duration(i) * time.Hour)))
		case fv.Kind() == reflect.String:
			fv.SetString(field.Name + "-value")
		case fv.Kind() == reflect.Float64:
			// Keep in [0,1]: Confidence and SourceAuthority are validated.
			fv.SetFloat(0.5 + float64(i)/1000)
		case fv.Kind() == reflect.Int:
			fv.SetInt(int64(i + 1))
		}
		if fv.IsZero() {
			t.Fatalf("fillClaim does not know how to fill domain.Claim.%s (%s); "+
				"add a case here (and a storedClaim slot) so the parity check stays honest",
				field.Name, field.Type)
		}
	}
	return c
}

// A storedClaim must carry every domain.Claim field. Dropping one is silent:
// the read path returns a zero value that downstream code treats as real data
// (an empty Visibility is coerced to "team", an empty TestRequirementRef makes
// ListByTestRequirementRef structurally unable to match). Reflection over the
// domain struct is deliberate — a hand-listed field set goes stale the first
// time someone adds a column.
func TestStoredClaim_RoundTripsEveryField(t *testing.T) {
	t.Parallel()
	want := fillClaim(t)

	got := storedClaimFromDomain(want).toDomain()

	wv := reflect.ValueOf(want)
	gv := reflect.ValueOf(got)
	typ := wv.Type()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if !reflect.DeepEqual(wv.Field(i).Interface(), gv.Field(i).Interface()) {
			t.Errorf("domain.Claim.%s did not survive the memory round trip: stored %#v, read back %#v",
				name, wv.Field(i).Interface(), gv.Field(i).Interface())
		}
	}
}

// The round trip must also hold through the repository, not just the record
// mapper — Upsert/ListByIDs are what callers actually touch.
func TestClaimRepository_UpsertPreservesEveryField(t *testing.T) {
	t.Parallel()
	st := newState()
	repo := ClaimRepository{state: st}
	want := fillClaim(t)
	want.ID = "claim-parity"

	if err := repo.Upsert(t.Context(), []domain.Claim{want}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.ListByIDs(t.Context(), []string{want.ID})
	if err != nil {
		t.Fatalf("list by ids: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}

	wv := reflect.ValueOf(want)
	gv := reflect.ValueOf(got[0])
	typ := wv.Type()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if !reflect.DeepEqual(wv.Field(i).Interface(), gv.Field(i).Interface()) {
			t.Errorf("domain.Claim.%s lost through Upsert→ListByIDs: wrote %#v, read %#v",
				name, wv.Field(i).Interface(), gv.Field(i).Interface())
		}
	}
}

// ListByTestRequirementRef was structurally dead on this backend: the filter
// requires a non-empty TestRequirementRef, and the field was never persisted,
// so no row could ever match and test-vs-test contradiction resolution silently
// returned nothing.
func TestClaimRepository_ListByTestRequirementRef_FindsPersistedRef(t *testing.T) {
	t.Parallel()
	st := newState()
	repo := ClaimRepository{state: st}
	ctx := t.Context()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	claims := []domain.Claim{
		{
			ID: "t-old", Text: "login test passed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogin/old", TestRequirementRef: "REQ-42", TestLastRunAt: now.Add(-48 * time.Hour),
		},
		{
			ID: "t-new", Text: "login test failed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogin/new", TestRequirementRef: "REQ-42", TestLastRunAt: now,
		},
		{
			ID: "t-other", Text: "logout test passed", Type: domain.ClaimTypeTestResult,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestID: "TestLogout", TestRequirementRef: "REQ-99", TestLastRunAt: now,
		},
		{
			ID: "not-a-test", Text: "billing runs on postgres", Type: domain.ClaimTypeFact,
			Confidence: 0.9, Status: domain.ClaimStatusActive, CreatedAt: now,
			TestRequirementRef: "REQ-42",
		},
	}
	if err := repo.Upsert(ctx, claims); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.ListByTestRequirementRef(ctx, "REQ-42")
	if err != nil {
		t.Fatalf("list by test requirement ref: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	// Freshest run first, and only test_result claims sharing the ref.
	if !reflect.DeepEqual(ids, []string{"t-new", "t-old"}) {
		t.Errorf("got %v, want [t-new t-old]", ids)
	}
}
