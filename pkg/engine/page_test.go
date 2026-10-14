package engine

import (
	"bytes"
	"errors"
	"testing"
)

func rec(id, slot, parent uint64, children []uint64, attrs ...string) *Record {
	return &Record{ID: id, Slot: slot, Parent: parent, Children: children, Attrs: attrs}
}

func TestPagePutGet(t *testing.T) {
	p := NewPage(2, 0)
	r := rec(1001, 5, 101, nil, "Alex", "alex@dev.io")
	if err := p.Put(r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := p.Get(5); got == nil || got.ID != 1001 {
		t.Fatalf("Get(5) = %+v", got)
	}
	if p.Get(4) != nil || p.Get(SlotsPerPage) != nil {
		t.Fatal("empty/out-of-window slots must return nil")
	}
	if !p.Valid(5) || p.Valid(4) {
		t.Fatal("validity bits wrong")
	}
	if p.Count() != 1 {
		t.Fatalf("Count = %d, want 1", p.Count())
	}
}

func TestPageSlotBaseAlignment(t *testing.T) {
	p := NewPage(1, 2048+3) // must snap to the page window
	if p.SlotBase != 2048 {
		t.Fatalf("SlotBase = %d, want 2048", p.SlotBase)
	}
	if err := p.Put(rec(1, 2048, 0, nil)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Put(rec(2, 3072, 0, nil)); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("out-of-window Put err = %v, want ErrCorruptPage", err)
	}
}

func TestPageInvalidateRangeWordParallel(t *testing.T) {
	p := NewPage(0, 0)
	for x := uint64(0); x < 300; x++ {
		if err := p.Put(rec(x+1, x, 0, nil)); err != nil {
			t.Fatal(err)
		}
	}
	// Punch out [60, 200): crosses three 64-bit bitmask words.
	p.InvalidateRange(60, 140)
	for x := uint64(0); x < 300; x++ {
		want := x < 60 || x >= 200
		if got := p.Valid(x); got != want {
			t.Fatalf("Valid(%d) = %v, want %v", x, got, want)
		}
	}
	if p.Count() != 160 {
		t.Fatalf("Count = %d, want 160", p.Count())
	}
	// Deleting an already-invalid range must be a no-op.
	p.InvalidateRange(60, 140)
	if p.Count() != 160 {
		t.Fatalf("Count after re-invalidate = %d, want 160", p.Count())
	}
	// Full-width word flip.
	p.InvalidateRange(0, 1024)
	if p.Count() != 0 {
		t.Fatalf("Count after full invalidate = %d, want 0", p.Count())
	}
}

func TestPageInvalidateRangeClamps(t *testing.T) {
	p := NewPage(0, 0)
	for x := uint64(1000); x < 1024; x++ {
		if err := p.Put(rec(x+1, x, 0, nil)); err != nil {
			t.Fatal(err)
		}
	}
	p.InvalidateRange(1020, 100) // extends past the window
	if p.Count() != 20 {
		t.Fatalf("Count = %d, want 20", p.Count())
	}
	p.InvalidateRange(0, 2000) // fully covering window
	if p.Count() != 0 {
		t.Fatalf("Count = %d, want 0", p.Count())
	}
}

func TestPageMarshalRoundTrip(t *testing.T) {
	p := NewPage(1, 1024)
	if err := p.Put(rec(10, 1024, 2, []uint64{7, 8}, "Eng Team")); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(rec(11, 1100, 2, nil, "Sales Team")); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(rec(12, 1200, 3, nil, "R&D Team")); err != nil {
		t.Fatal(err)
	}
	p.Invalidate(1100)

	buf, err := p.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	got, err := UnmarshalPage(buf)
	if err != nil {
		t.Fatalf("UnmarshalPage: %v", err)
	}
	if got.Layer != 1 || got.SlotBase != 1024 {
		t.Fatalf("header = layer %d base %d", got.Layer, got.SlotBase)
	}
	if got.Count() != 2 {
		t.Fatalf("Count = %d, want 2", got.Count())
	}
	r := got.Get(1024)
	if r == nil || r.ID != 10 || r.Attrs[0] != "Eng Team" || len(r.Children) != 2 || r.Children[1] != 8 {
		t.Fatalf("record = %+v", r)
	}
	if got.Get(1100) != nil {
		t.Fatal("invalid slot survived round trip")
	}
	if r := got.Get(1200); r == nil || r.Parent != 3 {
		t.Fatalf("record = %+v", r)
	}
}

func TestPageUnmarshalCorrupted(t *testing.T) {
	p := NewPage(0, 0)
	if err := p.Put(rec(1, 0, 0, nil, "x")); err != nil {
		t.Fatal(err)
	}
	good, err := p.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalPage(nil); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("empty page err = %v", err)
	}
	bad := append([]byte(nil), good...)
	bad[0] = 0 // bad magic
	if _, err := UnmarshalPage(bad); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("bad magic err = %v", err)
	}
	trunc := good[:len(good)-3]
	if _, err := UnmarshalPage(trunc); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("truncated err = %v", err)
	}
	bad = append([]byte(nil), good...)
	bad[16] = 99 // inflated tuple count
	if _, err := UnmarshalPage(bad); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("bad tuple count err = %v", err)
	}
}

func TestRecordCodecRoundTrip(t *testing.T) {
	r := rec(42, 7, 3, []uint64{1, 2, 3}, "a", "b:c", "")
	got, err := decodeRecord(encodeRecord(r))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || got.Slot != r.Slot || got.Parent != r.Parent {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if !bytes.Equal(encodeRecord(got), encodeRecord(r)) {
		t.Fatal("re-encoding mismatch")
	}
	if _, err := decodeRecord([]byte{1, 2, 3}); !errors.Is(err, ErrCorruptPage) {
		t.Fatalf("truncated record err = %v", err)
	}
}
