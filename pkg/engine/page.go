package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

const (
	pageMagic     = 0x50414745 // "PAGE"
	SlotsPerPage  = 1024       // contiguous X-axis slots per slotted page
	pageWords     = SlotsPerPage / 64
	pageFixedHead = 20 // magic(4) + layer(4) + slotBase(8) + tupleCount(4)
)

// ErrCorruptPage is returned when a slotted page cannot be decoded.
var ErrCorruptPage = errors.New("engine: corrupt slotted page")

// Record is one matrix cell: a hierarchy node with parent/child pointers and
// length-prefixed scalar attributes. Records are treated as immutable once
// published in a snapshot; writers always clone before mutating.
type Record struct {
	ID       uint64   `json:"id"`
	Slot     uint64   `json:"slot"`   // X: contiguous offset slot within the layer
	Parent   uint64   `json:"parent"` // 0 = no parent (Parent:NONE)
	Children []uint64 `json:"children"`
	Attrs    []string `json:"attrs"`
}

// Clone returns a deep copy of the record.
func (r *Record) Clone() *Record {
	c := *r
	if r.Children != nil {
		c.Children = append([]uint64(nil), r.Children...)
	}
	if r.Attrs != nil {
		c.Attrs = append([]string(nil), r.Attrs...)
	}
	return &c
}

// Page is a slotted binary page: a contiguous window of SlotsPerPage X-axis
// slots of one layer, plus the validity bitmask used for range invalidation
// (cascade deletes) without sweeping the tuple data.
type Page struct {
	Layer    int
	SlotBase uint64 // X of slot 0 in this page
	valid    [pageWords]uint64
	slots    [SlotsPerPage]*Record
}

// NewPage returns an empty page covering [slotBase, slotBase+SlotsPerPage).
func NewPage(layer int, slotBase uint64) *Page {
	return &Page{Layer: layer, SlotBase: slotBase - slotBase%SlotsPerPage}
}

// slotIndex maps an absolute slot X to a page-local index.
func (p *Page) slotIndex(x uint64) (int, bool) {
	if x < p.SlotBase || x >= p.SlotBase+SlotsPerPage {
		return 0, false
	}
	return int(x - p.SlotBase), true
}

// Valid reports whether the slot's validity bit is set.
func (p *Page) Valid(x uint64) bool {
	i, ok := p.slotIndex(x)
	if !ok {
		return false
	}
	return p.valid[i/64]&(1<<uint(i%64)) != 0
}

// Get returns the record in slot x, or nil when the slot is empty/invalid.
func (p *Page) Get(x uint64) *Record {
	if !p.Valid(x) {
		return nil
	}
	i, _ := p.slotIndex(x)
	return p.slots[i]
}

// Put stores a record at its own Slot position and marks the slot valid.
func (p *Page) Put(rec *Record) error {
	i, ok := p.slotIndex(rec.Slot)
	if !ok {
		return fmt.Errorf("%w: slot %d outside page window %d", ErrCorruptPage, rec.Slot, p.SlotBase)
	}
	p.slots[i] = rec
	p.valid[i/64] |= 1 << uint(i%64)
	return nil
}

// Invalidate flips the validity bit of one slot and drops the tuple pointer.
func (p *Page) Invalidate(x uint64) {
	i, ok := p.slotIndex(x)
	if !ok || p.valid[i/64]&(1<<uint(i%64)) == 0 {
		return
	}
	p.valid[i/64] &^= 1 << uint(i%64)
	p.slots[i] = nil
}

// InvalidateRange flips the validity bitmask for the contiguous slot range
// [start, start+count) using word-parallel bit operations, then drops the
// tuple pointers. Ranges are clamped to the page window. This is the core of
// the O(1) matrix cascade: contiguous child slots are pruned with a constant
// number of bitmask word writes per page, with zero tuple or index sweeps.
func (p *Page) InvalidateRange(start, count uint64) {
	if count == 0 {
		return
	}
	end := start + count
	if start < p.SlotBase {
		start = p.SlotBase
	}
	if end > p.SlotBase+SlotsPerPage {
		end = p.SlotBase + SlotsPerPage
	}
	if start >= end {
		return
	}
	for x := start; x < end; {
		i, _ := p.slotIndex(x)
		word := i / 64
		bit := uint(i % 64)
		take := uint(64 - bit)
		if remaining := end - x; remaining < uint64(take) {
			take = uint(remaining)
		}
		var mask uint64
		if take == 64 {
			mask = ^uint64(0)
		} else {
			mask = ((uint64(1) << take) - 1) << bit
		}
		cleared := p.valid[word] & mask
		p.valid[word] &^= mask
		for b := uint(0); b < take; b++ {
			if cleared&(1<<b) != 0 {
				p.slots[i+int(b)] = nil
			}
		}
		x += uint64(take)
	}
}

// Count returns the number of valid slots in the page.
func (p *Page) Count() int {
	n := 0
	for word, bits := range p.valid {
		for bits != 0 {
			bits &= bits - 1
			n++
		}
		_ = word
	}
	return n
}

// Slots returns the valid records in ascending slot order.
func (p *Page) Slots() []*Record {
	out := make([]*Record, 0, p.Count())
	for i := 0; i < SlotsPerPage; i++ {
		if p.valid[i/64]&(1<<uint(i%64)) != 0 {
			out = append(out, p.slots[i])
		}
	}
	return out
}

// visit calls fn for each valid record. reverse walks the page from the
// high slot down, which is descending id order within a layer. The walk
// stops when fn returns false.
func (p *Page) visit(reverse bool, fn func(*Record) bool) bool {
	if p == nil {
		return true
	}
	i, step := 0, 1
	if reverse {
		i, step = SlotsPerPage-1, -1
	}
	for n := 0; n < SlotsPerPage; n++ {
		if p.valid[i/64]&(1<<uint(i%64)) != 0 && p.slots[i] != nil {
			if !fn(p.slots[i]) {
				return false
			}
		}
		i += step
	}
	return true
}

// Clone returns a deep copy of the page (copy-on-write helper).
func (p *Page) Clone() *Page {
	c := &Page{Layer: p.Layer, SlotBase: p.SlotBase}
	c.valid = p.valid
	c.slots = p.slots
	return c
}

// MarshalBinary encodes the page in slotted layout:
//
//	magic(4) layer(4) slotBase(8) tupleCount(4) | validity bitmap | slot dir | tuples
//
// The slot directory holds (slotIdx u16, tupleLen u32) pairs in ascending
// slot order; tuples follow immediately after.
func (p *Page) MarshalBinary() ([]byte, error) {
	return p.marshalBinary(true)
}

func (p *Page) marshalBinary(wide bool) ([]byte, error) {
	type entry struct {
		idx  int
		data []byte
	}
	var entries []entry
	for i := 0; i < SlotsPerPage; i++ {
		if p.valid[i/64]&(1<<uint(i%64)) == 0 {
			continue
		}
		rec := p.slots[i]
		if rec == nil {
			return nil, fmt.Errorf("%w: valid slot %d has no tuple", ErrCorruptPage, p.SlotBase+uint64(i))
		}
		data, err := encodeRecord(rec, wide)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{idx: i, data: data})
	}

	size := pageFixedHead + pageWords*8 + len(entries)*6
	for _, e := range entries {
		size += len(e.data)
	}
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:4], pageMagic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(p.Layer))
	binary.LittleEndian.PutUint64(buf[8:16], p.SlotBase)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(entries)))

	off := pageFixedHead
	for w := 0; w < pageWords; w++ {
		binary.LittleEndian.PutUint64(buf[off:off+8], p.valid[w])
		off += 8
	}
	dirStart := off
	off += len(entries) * 6
	for i, e := range entries {
		binary.LittleEndian.PutUint16(buf[dirStart+i*6:], uint16(e.idx))
		binary.LittleEndian.PutUint32(buf[dirStart+i*6+2:], uint32(len(e.data)))
		copy(buf[off:], e.data)
		off += len(e.data)
	}
	return buf, nil
}

// UnmarshalPage decodes a slotted page produced by MarshalBinary (v1.1
// wide tuples). v1.0 pages are decoded by unmarshalPage with wide=false.
func UnmarshalPage(buf []byte) (*Page, error) {
	return unmarshalPage(buf, true)
}

// unmarshalPage decodes a slotted page. wide selects the v1.1 tuple
// layout (uint32 lengths); v1.0 pages use uint16 lengths.
func unmarshalPage(buf []byte, wide bool) (*Page, error) {
	if len(buf) < pageFixedHead+pageWords*8 {
		return nil, fmt.Errorf("%w: page too short (%d bytes)", ErrCorruptPage, len(buf))
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != pageMagic {
		return nil, fmt.Errorf("%w: bad page magic", ErrCorruptPage)
	}
	p := &Page{
		Layer:    int(int32(binary.LittleEndian.Uint32(buf[4:8]))),
		SlotBase: binary.LittleEndian.Uint64(buf[8:16]),
	}
	if p.Layer < 0 {
		return nil, fmt.Errorf("%w: negative layer %d", ErrCorruptPage, p.Layer)
	}
	if p.SlotBase%SlotsPerPage != 0 {
		return nil, fmt.Errorf("%w: unaligned slot base %d", ErrCorruptPage, p.SlotBase)
	}
	tupleCount := int(binary.LittleEndian.Uint32(buf[16:20]))
	if tupleCount > SlotsPerPage {
		return nil, fmt.Errorf("%w: tuple count %d exceeds page slots", ErrCorruptPage, tupleCount)
	}
	off := pageFixedHead
	for w := 0; w < pageWords; w++ {
		p.valid[w] = binary.LittleEndian.Uint64(buf[off : off+8])
		off += 8
	}
	dirBytes := tupleCount * 6
	if len(buf) < off+dirBytes {
		return nil, fmt.Errorf("%w: truncated slot directory", ErrCorruptPage)
	}
	dirStart := off
	off += dirBytes
	bits := 0
	for w := 0; w < pageWords; w++ {
		for v := p.valid[w]; v != 0; v &= v - 1 {
			bits++
		}
	}
	if bits != tupleCount {
		return nil, fmt.Errorf("%w: validity bitmask has %d bits, want %d", ErrCorruptPage, bits, tupleCount)
	}
	for i := 0; i < tupleCount; i++ {
		idx := int(binary.LittleEndian.Uint16(buf[dirStart+i*6:]))
		tupleLen := int(binary.LittleEndian.Uint32(buf[dirStart+i*6+2:]))
		if idx >= SlotsPerPage {
			return nil, fmt.Errorf("%w: slot index %d out of range", ErrCorruptPage, idx)
		}
		if tupleLen <= 0 || len(buf) < off+tupleLen {
			return nil, fmt.Errorf("%w: truncated tuple at slot %d", ErrCorruptPage, idx)
		}
		rec, err := decodeRecord(buf[off:off+tupleLen], wide)
		if err != nil {
			return nil, err
		}
		off += tupleLen
		if rec.Slot != p.SlotBase+uint64(idx) {
			return nil, fmt.Errorf("%w: tuple slot %d does not match directory slot %d", ErrCorruptPage, rec.Slot, p.SlotBase+uint64(idx))
		}
		if p.valid[idx/64]&(1<<uint(idx%64)) == 0 {
			return nil, fmt.Errorf("%w: tuple at slot %d not marked valid", ErrCorruptPage, idx)
		}
		p.slots[idx] = rec
	}
	return p, nil
}

// encodeRecord serializes one record tuple.
//
// v1.1 (wide): id(8) slot(8) parent(8) nChildren(4) children(8*) nAttrs(4) attrs(4+len*)
// v1.0:        the three length fields are uint16. Callers must not pass a
// record that does not fit; this returns an error instead of truncating,
// which is what used to make the file unopenable (issue #44).
func encodeRecord(r *Record, wide bool) ([]byte, error) {
	if err := recordFits(r, wide); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	writeU64 := func(v uint64) {
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], v)
		b.Write(tmp[:])
	}
	writeU32 := func(v uint32) {
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], v)
		b.Write(tmp[:])
	}
	writeU16 := func(v uint16) {
		var tmp [2]byte
		binary.LittleEndian.PutUint16(tmp[:], v)
		b.Write(tmp[:])
	}
	writeCount := func(n int) {
		if wide {
			writeU32(uint32(n))
			return
		}
		writeU16(uint16(n))
	}
	writeU64(r.ID)
	writeU64(r.Slot)
	writeU64(r.Parent)
	writeCount(len(r.Children))
	for _, c := range r.Children {
		writeU64(c)
	}
	writeCount(len(r.Attrs))
	for _, a := range r.Attrs {
		writeCount(len(a))
		b.WriteString(a)
	}
	return b.Bytes(), nil
}

// recordFits reports whether every length in r fits the tuple width.
func recordFits(r *Record, wide bool) error {
	limit := uint64(math.MaxUint32)
	if !wide {
		limit = math.MaxUint16
	}
	if uint64(len(r.Children)) > limit {
		return fmt.Errorf("engine: record %d has %d children, max %d", r.ID, len(r.Children), limit)
	}
	if uint64(len(r.Attrs)) > limit {
		return fmt.Errorf("engine: record %d has %d attributes, max %d", r.ID, len(r.Attrs), limit)
	}
	for i, a := range r.Attrs {
		if uint64(len(a)) > limit {
			return fmt.Errorf("engine: attribute %d of record %d is %d bytes, max %d", i, r.ID, len(a), limit)
		}
	}
	return nil
}

func decodeRecord(b []byte, wide bool) (*Record, error) {
	r := &Record{}
	rd := bytes.NewReader(b)
	readU64 := func(dst *uint64) error {
		var tmp [8]byte
		if _, err := io.ReadFull(rd, tmp[:]); err != nil {
			return fmt.Errorf("%w: truncated record: %v", ErrCorruptPage, err)
		}
		*dst = binary.LittleEndian.Uint64(tmp[:])
		return nil
	}
	readCount := func() (uint64, error) {
		if wide {
			var tmp [4]byte
			if _, err := io.ReadFull(rd, tmp[:]); err != nil {
				return 0, fmt.Errorf("%w: truncated record: %v", ErrCorruptPage, err)
			}
			return uint64(binary.LittleEndian.Uint32(tmp[:])), nil
		}
		var tmp [2]byte
		if _, err := io.ReadFull(rd, tmp[:]); err != nil {
			return 0, fmt.Errorf("%w: truncated record: %v", ErrCorruptPage, err)
		}
		return uint64(binary.LittleEndian.Uint16(tmp[:])), nil
	}
	if err := readU64(&r.ID); err != nil {
		return nil, err
	}
	if err := readU64(&r.Slot); err != nil {
		return nil, err
	}
	if err := readU64(&r.Parent); err != nil {
		return nil, err
	}
	nChild, err := readCount()
	if err != nil {
		return nil, err
	}
	// Reject a count that cannot fit in the bytes still unread before
	// allocating. Each child is 8 bytes.
	if nChild > uint64(rd.Len()/8) {
		return nil, fmt.Errorf("%w: child count %d exceeds record", ErrCorruptPage, nChild)
	}
	r.Children = make([]uint64, nChild)
	for i := range r.Children {
		if err := readU64(&r.Children[i]); err != nil {
			return nil, err
		}
	}
	nAttr, err := readCount()
	if err != nil {
		return nil, err
	}
	minAttr := 2
	if wide {
		minAttr = 4
	}
	if minAttr > 0 && nAttr > uint64(rd.Len()/minAttr) {
		return nil, fmt.Errorf("%w: attribute count %d exceeds record", ErrCorruptPage, nAttr)
	}
	r.Attrs = make([]string, nAttr)
	for i := range r.Attrs {
		n, err := readCount()
		if err != nil {
			return nil, err
		}
		if uint64(rd.Len()) < n {
			return nil, fmt.Errorf("%w: truncated attribute", ErrCorruptPage)
		}
		tmp := make([]byte, n)
		if _, err := io.ReadFull(rd, tmp); err != nil {
			return nil, fmt.Errorf("%w: truncated attribute: %v", ErrCorruptPage, err)
		}
		r.Attrs[i] = string(tmp)
	}
	if rd.Len() != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes in record", ErrCorruptPage, rd.Len())
	}
	return r, nil
}

// sortedSlots is a small helper used by cascade range grouping.
func sortedSlots(slots []uint64) []uint64 {
	out := append([]uint64(nil), slots...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
