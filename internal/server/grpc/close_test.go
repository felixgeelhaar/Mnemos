package grpc

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/bolt"
	mnemos "go.klarlabs.de/mnemos"
	"go.klarlabs.de/mnemos/internal/store"
	_ "go.klarlabs.de/mnemos/internal/store/memory"
)

// countingMemory records Close calls. The embedded nil interface satisfies
// mnemos.Memory without implementing its ~100 methods; nothing here calls any
// of them, and a test that accidentally did would panic loudly rather than
// pass on a stub that quietly returns zero values.
type countingMemory struct {
	mnemos.Memory
	closes int
	err    error
}

func (m *countingMemory) Close() error {
	m.closes++
	return m.err
}

// TestServer_CloseReleasesTenantMemories: memFor caches one Memory per tenant
// and each holds a store connection. The type had no Close at all, so a
// multi-tenant process accumulated one live connection per tenant it had ever
// served, for the lifetime of the process — the HTTP surface released its
// equivalent map, this one had no way to be released.
func TestServer_CloseReleasesTenantMemories(t *testing.T) {
	conn, err := store.Open(context.Background(), "memory://close-test")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = conn.Close() }()

	s := NewServer(conn, nil, bolt.New(bolt.NewJSONHandler(nil)), "test")
	a := &countingMemory{}
	b := &countingMemory{}
	s.tenantMems["acme"] = a
	s.tenantMems["globex"] = b

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.closes != 1 || b.closes != 1 {
		t.Errorf("closes = (%d, %d), want every cached tenant Memory closed exactly once", a.closes, b.closes)
	}

	// Idempotent: a second Close (a deferred one after an explicit one, say)
	// must not double-close a Memory that is already gone.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if a.closes != 1 || b.closes != 1 {
		t.Errorf("closes = (%d, %d) after a second Close; it is not idempotent", a.closes, b.closes)
	}
	if len(s.tenantMems) != 0 {
		t.Errorf("cache still holds %d entries after Close", len(s.tenantMems))
	}
}

// A failing tenant Close must not swallow the others: every cached Memory is
// closed and the errors are joined.
func TestServer_CloseReportsAllErrors(t *testing.T) {
	conn, err := store.Open(context.Background(), "memory://close-err-test")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = conn.Close() }()

	s := NewServer(conn, nil, bolt.New(bolt.NewJSONHandler(nil)), "test")
	boom := errors.New("boom")
	bad := &countingMemory{err: boom}
	good := &countingMemory{}
	s.tenantMems["acme"] = bad
	s.tenantMems["globex"] = good

	err = s.Close()
	if !errors.Is(err, boom) {
		t.Errorf("Close error = %v, want it to wrap the tenant failure", err)
	}
	if good.closes != 1 {
		t.Error("a failing tenant Close aborted the rest of the sweep")
	}
}
