package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBranch is created with every database.
const DefaultBranch = "main"

// Errors surfaced by the store.
var (
	ErrConflict    = errors.New("engine: concurrent write conflict")
	ErrTxDone      = errors.New("engine: transaction already finished")
	ErrBranchTaken = errors.New("engine: branch already exists")
	ErrLocked      = errors.New("engine: database is locked")
)

// TxMeta carries the audit metadata recorded with every commit and
// branch event. It is required for structural operations (container
// initialization, branch creation) and optional for record writes,
// where it stamps the commit block when present.
type TxMeta struct {
	Author  string `json:"author"`
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

// Validate enforces the audit fields for structural operations.
func (m TxMeta) Validate() error {
	if strings.TrimSpace(m.Author) == "" {
		return errors.New("engine: Author is required")
	}
	if strings.TrimSpace(m.Summary) == "" {
		return errors.New("engine: Summary is required")
	}
	if strings.TrimSpace(m.Reason) == "" {
		return errors.New("engine: Reason is required")
	}
	return nil
}

// Mutation kinds recorded in the journal for deterministic replay.
const (
	opCreate byte = 1
	opUpdate byte = 2
	opDelete byte = 3
)

// Op is one recorded mutation inside a commit.
type Op struct {
	Kind   byte
	Layer  int
	Record *Record // for opCreate / opUpdate
	ID     uint64  // for opDelete (root of the cascade)
}

// maxOverlayDepth bounds how many published copy-on-write layers may stack
// before a commit flattens them into one root. Deeper chains make every
// read walk more maps; shallower ones make flattening too frequent. Each
// flatten copies the whole layer, so the amortized per-commit copy work is
// O(layer size / maxOverlayDepth) instead of O(layer size) per commit.
const maxOverlayDepth = 256

// layerState is the physical state of one Y-axis layer: slotted pages keyed
// by page index (slot / SlotsPerPage), the ID -> slot index, and the next
// contiguous X slot to allocate.
//
// States are layered copy-on-write overlays. A transaction's state points
// at the published state it started from (base) and records only what it
// changed: pages touched, ids added, ids deleted. Published states are
// immutable, so Begin is O(1) regardless of database size (issue #34) and
// older snapshots keep seeing their own state. Commit publishes the overlay
// as-is while it is shallow and sparse, and flattens it into a root
// otherwise; because flattening starts once the accumulated deltas rival
// the base, the total copy work over a stream of commits stays amortized
// O(1) per commit.
type layerState struct {
	base     *layerState           // shared immutable parent (nil for a root)
	pages    map[uint64]*Page      // pages created/modified relative to base
	idIndex  map[uint64]uint64     // ids added relative to base
	dels     map[uint64]bool       // ids deleted relative to base (tombstones)
	indexes  map[string]*attrIndex // attr name -> COW hash index tip at this level
	disk     *diskLayer            // immutable page directory (roots only)
	idDisk   *idDisk               // sorted id→slot pairs mapped from the file (roots only)
	nextSlot uint64
	count    int // live records, maintained across the overlay chain
}

func newLayerState() *layerState {
	return &layerState{
		pages:   map[uint64]*Page{},
		idIndex: map[uint64]uint64{},
		indexes: map[string]*attrIndex{},
	}
}

// bindSchema creates an empty hash index for every declared indexed
// attribute so equality lookups hit the index even when the layer is empty.
func (l *layerState) bindSchema(def Layer) {
	if l.indexes == nil {
		l.indexes = map[string]*attrIndex{}
	}
	for _, name := range def.Indexed {
		pos, ok := def.AttrIndex(name)
		if !ok {
			continue
		}
		canon := def.Attrs[pos]
		if l.indexes[canon] == nil {
			l.indexes[canon] = newAttrIndex()
		}
	}
}

// absorb indexes one decoded record into a root that is not published yet
// (open-time rebuild). It does not touch pages.
func (l *layerState) absorb(def Layer, rec *Record) {
	l.idIndex[rec.ID] = rec.Slot
	if rec.Slot >= l.nextSlot {
		l.nextSlot = rec.Slot + 1
	}
	l.count++
	l.indexInsert(def, rec)
}

func (l *layerState) indexInsert(def Layer, rec *Record) {
	if rec == nil {
		return
	}
	for _, name := range def.Indexed {
		pos, ok := def.AttrIndex(name)
		if !ok || pos >= len(rec.Attrs) {
			continue
		}
		l.indexForWrite(def.Attrs[pos]).add(rec.Attrs[pos], rec.ID)
	}
}

func (l *layerState) indexRemove(def Layer, rec *Record) {
	if rec == nil {
		return
	}
	for _, name := range def.Indexed {
		pos, ok := def.AttrIndex(name)
		if !ok || pos >= len(rec.Attrs) {
			continue
		}
		l.indexForWrite(def.Attrs[pos]).drop(rec.Attrs[pos], rec.ID)
	}
}

// tipIndex returns the newest index node for name, walking the overlay
// chain. nil means the attribute is not indexed.
func (l *layerState) tipIndex(name string) *attrIndex {
	canon := name
	for cur := l; cur != nil; cur = cur.base {
		if cur.indexes == nil {
			continue
		}
		if ix := cur.indexes[name]; ix != nil {
			return ix
		}
		for n, ix := range cur.indexes {
			if strings.EqualFold(n, name) && ix != nil {
				canon = n
				return cur.indexes[canon]
			}
		}
	}
	return nil
}

// indexForWrite returns an index node this layer may mutate. A node shared
// with the parent is wrapped first so a published snapshot's put map stays
// immutable.
func (l *layerState) indexForWrite(name string) *attrIndex {
	if l.indexes == nil {
		l.indexes = map[string]*attrIndex{}
	}
	var parent *attrIndex
	if l.base != nil {
		parent = l.base.tipIndex(name)
	}
	cur := l.indexes[name]
	if cur == nil || cur == parent {
		wrapped := &attrIndex{base: parent, put: map[string]map[uint64]struct{}{}}
		if cur != nil {
			wrapped.base = cur
		}
		l.indexes[name] = wrapped
		return wrapped
	}
	return cur
}

// idsByAttr resolves an equality lookup. The bool is false when name has
// no index (the caller should scan). An empty slice with true is a miss.
func (l *layerState) idsByAttr(name, val string) ([]uint64, bool) {
	if l == nil {
		return nil, false
	}
	tip := l.tipIndex(name)
	if tip == nil {
		return nil, false
	}
	return tip.lookup(val), true
}

// overlay returns a writable, initially empty copy-on-write layer on top of
// l. O(1): the base state is shared, not copied.
func (l *layerState) overlay() *layerState {
	if l == nil {
		return newLayerState()
	}
	indexes := make(map[string]*attrIndex, len(l.indexes))
	for k, v := range l.indexes {
		indexes[k] = v
	}
	return &layerState{
		base:     l,
		pages:    map[uint64]*Page{},
		idIndex:  map[uint64]uint64{},
		indexes:  indexes,
		nextSlot: l.nextSlot,
		count:    l.count,
	}
}

// page returns the page with the given index. Overlay pages win. A miss
// loads from the page directory into the shared cache and does not write
// the published pages map.
func (l *layerState) page(idx uint64) *Page {
	var disk *diskLayer
	var loc pageLoc
	var onDisk bool
	for cur := l; cur != nil; cur = cur.base {
		if p := cur.pages[idx]; p != nil {
			return p
		}
		if !onDisk && cur.disk != nil {
			if pl, ok := cur.disk.refs[idx]; ok {
				disk, loc, onDisk = cur.disk, pl, true
			}
		}
	}
	if !onDisk {
		return nil
	}
	p, err := disk.load(idx, loc)
	if err != nil {
		disk.gen.noteErr(err)
		return nil
	}
	return p
}

// onDisk reports whether any level has a directory entry for idx.
func (l *layerState) onDisk(idx uint64) bool {
	for ; l != nil; l = l.base {
		if l.disk != nil {
			if _, ok := l.disk.refs[idx]; ok {
				return true
			}
		}
	}
	return false
}

// pageIndexes returns every page index visible through the overlay chain.
func (l *layerState) pageIndexes() []uint64 {
	seen := map[uint64]struct{}{}
	var idxs []uint64
	for cur := l; cur != nil; cur = cur.base {
		for idx := range cur.pages {
			if _, ok := seen[idx]; ok {
				continue
			}
			seen[idx] = struct{}{}
			idxs = append(idxs, idx)
		}
		if cur.disk == nil {
			continue
		}
		for idx := range cur.disk.refs {
			if _, ok := seen[idx]; ok {
				continue
			}
			seen[idx] = struct{}{}
			idxs = append(idxs, idx)
		}
	}
	return idxs
}

func (l *layerState) loadErr() error {
	for ; l != nil; l = l.base {
		if l.disk != nil && l.disk.gen != nil {
			if err := l.disk.gen.err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// slotOf resolves an id to its slot through the overlay chain. A disk id
// table is binary-searched and is not copied into the overlay map.
func (l *layerState) slotOf(id uint64) (uint64, bool) {
	for ; l != nil; l = l.base {
		if x, ok := l.idIndex[id]; ok {
			return x, true
		}
		if l.dels[id] {
			return 0, false
		}
		if l.idDisk != nil {
			if x, ok := l.idDisk.find(id); ok {
				return x, true
			}
		}
	}
	return 0, false
}

func (l *layerState) record(id uint64) *Record {
	x, ok := l.slotOf(id)
	if !ok {
		return nil
	}
	p := l.page(x / SlotsPerPage)
	if p == nil {
		return nil
	}
	return p.Get(x)
}

// dropID removes an id from the index. An overlay records a tombstone so
// base entries are shadowed; a root drops the entry directly.
func (l *layerState) dropID(id uint64) {
	if _, ok := l.slotOf(id); !ok {
		return
	}
	l.count--
	delete(l.idIndex, id)
	if l.base == nil {
		return
	}
	if l.dels == nil {
		l.dels = map[uint64]bool{}
	}
	l.dels[id] = true
}

// materialize flattens the overlay chain into a self-contained root state;
// a root is returned unchanged. The root's maps are copied exactly once
// and each level's deltas applied on top (a recursive copy per level would
// cost O(depth x size)).
func (l *layerState) materialize() *layerState {
	if l == nil || l.base == nil {
		return l
	}
	// chain[0] is the leaf, the last entry the root.
	var chain []*layerState
	for cur := l; cur != nil; cur = cur.base {
		chain = append(chain, cur)
	}
	root := chain[len(chain)-1]
	if root.idDisk != nil {
		return l.materializeOverDisk(chain, root)
	}
	c := &layerState{
		pages:    make(map[uint64]*Page, len(root.pages)+len(l.pages)),
		idIndex:  make(map[uint64]uint64, len(root.idIndex)+len(l.idIndex)),
		indexes:  flattenIndexMap(l),
		disk:     root.disk,
		nextSlot: l.nextSlot,
		count:    l.count,
	}
	for k, v := range root.pages {
		c.pages[k] = v
	}
	for k, v := range root.idIndex {
		c.idIndex[k] = v
	}
	for i := len(chain) - 2; i >= 0; i-- {
		lvl := chain[i]
		for k, v := range lvl.pages {
			c.pages[k] = v
		}
		for k, v := range lvl.idIndex {
			c.idIndex[k] = v
		}
		for id := range lvl.dels {
			delete(c.idIndex, id)
		}
	}
	return c
}

// materializeOverDisk collapses memory deltas into one overlay that still
// points at the mapped id table. It does not load that table into a map.
func (l *layerState) materializeOverDisk(chain []*layerState, root *layerState) *layerState {
	c := &layerState{
		base:     root,
		pages:    make(map[uint64]*Page, len(root.pages)+len(l.pages)),
		idIndex:  make(map[uint64]uint64, len(root.idIndex)+len(l.idIndex)),
		indexes:  flattenIndexMap(l),
		nextSlot: l.nextSlot,
		count:    l.count,
	}
	for k, v := range root.pages {
		c.pages[k] = v
	}
	for k, v := range root.idIndex {
		c.idIndex[k] = v
	}
	for i := len(chain) - 2; i >= 0; i-- {
		lvl := chain[i]
		for k, v := range lvl.pages {
			c.pages[k] = v
		}
		for k, v := range lvl.idIndex {
			c.idIndex[k] = v
			if c.dels != nil {
				delete(c.dels, k)
			}
		}
		for id := range lvl.dels {
			delete(c.idIndex, id)
			if c.dels == nil {
				c.dels = map[uint64]bool{}
			}
			c.dels[id] = true
		}
	}
	return c
}

// flattenIndexMap copies every live secondary index into a root node.
func flattenIndexMap(l *layerState) map[string]*attrIndex {
	names := map[string]struct{}{}
	for cur := l; cur != nil; cur = cur.base {
		for n := range cur.indexes {
			names[n] = struct{}{}
		}
	}
	out := make(map[string]*attrIndex, len(names))
	for n := range names {
		if tip := l.tipIndex(n); tip != nil {
			out[n] = tip.flatten()
		}
	}
	return out
}

// copyRoot returns a deep copy of a materialized state whose pages may keep
// being mutated in place by an unpublished view (journal replay forks).
func (l *layerState) copyRoot() *layerState {
	m := l.materialize()
	c := &layerState{
		base:     m.base,
		pages:    make(map[uint64]*Page, len(m.pages)),
		idIndex:  make(map[uint64]uint64, len(m.idIndex)),
		indexes:  flattenIndexMap(m),
		disk:     m.disk,
		idDisk:   m.idDisk,
		nextSlot: m.nextSlot,
		count:    m.count,
	}
	// The disk id table and page directory stay on the shared root. The
	// copy holds only the in-memory deltas so later replay cannot mutate
	// the fork through the source.
	if m.base != nil {
		c.disk = nil
		c.idDisk = nil
	}
	for k, v := range m.pages {
		c.pages[k] = v
	}
	for k, v := range m.idIndex {
		c.idIndex[k] = v
	}
	if len(m.dels) > 0 {
		c.dels = make(map[uint64]bool, len(m.dels))
		for k, v := range m.dels {
			c.dels[k] = v
		}
	}
	return c
}

// depth counts the layers stacked on top of a nil base.
func (l *layerState) depth() int {
	n := 0
	for ; l != nil; l = l.base {
		n++
	}
	return n
}

// settled returns the state to publish for this overlay: the overlay while
// it is shallow and sparse, otherwise a flattened root. Flattening once
// the accumulated deltas rival the base keeps the copy work amortized O(1)
// per commit (sizes double, so copies form a geometric series).
func (l *layerState) settled() *layerState {
	if l.base == nil {
		return l
	}
	if l.depth() >= maxOverlayDepth || 4*(len(l.idIndex)+len(l.dels)) >= l.count {
		return l.materialize()
	}
	return l
}

// matrixView is a read-only hierarchical matrix over the layer states of one
// branch at one commit. All views published in snapshots are immutable.
type matrixView struct {
	hdr    *Header
	layers map[int]*layerState
}

// Layers returns the layer definitions in topological (height) order.
func (mv *matrixView) Layers() []Layer {
	heights := mv.hdr.LayerHeights()
	out := make([]Layer, 0, len(heights))
	for _, h := range heights {
		out = append(out, mv.hdr.Layers[h])
	}
	return out
}

func (mv *matrixView) height(name string) (int, error) {
	l, ok := mv.hdr.LayerByName(name)
	if !ok {
		names := make([]string, 0, len(mv.hdr.Layers))
		for _, l := range mv.hdr.Layers {
			names = append(names, l.Name)
		}
		sort.Strings(names)
		return 0, fmt.Errorf("engine: unknown layer %q (have %s)", name, strings.Join(names, ", "))
	}
	return l.Height, nil
}

// Record returns the record with the given ID at the given layer height.
func (mv *matrixView) Record(layer int, id uint64) (*Record, bool) {
	ls := mv.layers[layer]
	if ls == nil {
		return nil, false
	}
	r := ls.record(id)
	return r, r != nil
}

// RecordBySlot returns the record occupying X-axis slot x of a layer.
func (mv *matrixView) RecordBySlot(layer int, x uint64) (*Record, bool) {
	ls := mv.layers[layer]
	if ls == nil {
		return nil, false
	}
	p := ls.page(x / SlotsPerPage)
	if p == nil {
		return nil, false
	}
	r := p.Get(x)
	return r, r != nil
}

// Records returns all live records of a layer in slot order. Pages are
// already slot-ordered, so this walks them directly instead of collecting
// and sorting every record (issue #33).
func (mv *matrixView) Records(layer int) []*Record {
	ls := mv.layers[layer]
	if ls == nil {
		return nil
	}
	idxs := ls.pageIndexes()
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	var out []*Record
	for _, idx := range idxs {
		p := ls.page(idx)
		if p == nil {
			continue
		}
		out = append(out, p.Slots()...)
	}
	return out
}

// Walk visits live records of a layer in slot order and stops when fn
// returns false. reverse walks high slots first, which is descending id
// order within a layer because ids are assigned alongside slots.
func (mv *matrixView) Walk(layer int, reverse bool, fn func(*Record) bool) {
	ls := mv.layers[layer]
	if ls == nil {
		return
	}
	idxs := ls.pageIndexes()
	if reverse {
		sort.Slice(idxs, func(i, j int) bool { return idxs[i] > idxs[j] })
	} else {
		sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	}
	for _, idx := range idxs {
		p := ls.page(idx)
		if p == nil {
			continue
		}
		if !p.visit(reverse, fn) {
			return
		}
	}
}

// Roots returns the records of a layer without a parent (matrix row heads).
func (mv *matrixView) Roots(layer int) []*Record {
	var out []*Record
	for _, r := range mv.Records(layer) {
		if r.Parent == 0 {
			out = append(out, r)
		}
	}
	return out
}

// Children returns the child records of a node. Per the matrix invariant,
// children of a layer height Y live at height Y+1.
func (mv *matrixView) Children(layer int, id uint64) []*Record {
	rec, ok := mv.Record(layer, id)
	if !ok || len(rec.Children) == 0 {
		return nil
	}
	ls := mv.layers[layer+1]
	if ls == nil {
		return nil
	}
	out := make([]*Record, 0, len(rec.Children))
	for _, c := range rec.Children {
		if r := ls.record(c); r != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// IDsByAttr returns ids whose indexed attribute equals val, in ascending
// id order. The bool is false when the attribute has no hash index.
func (mv *matrixView) IDsByAttr(layer int, field, val string) ([]uint64, bool) {
	ls := mv.layers[layer]
	if ls == nil {
		return nil, false
	}
	name := field
	if def, ok := mv.hdr.Layers[layer]; ok {
		if pos, err := strconv.Atoi(field); err == nil && pos >= 0 && pos < len(def.Attrs) {
			name = def.Attrs[pos]
		} else if p, ok := def.AttrIndex(field); ok {
			name = def.Attrs[p]
		}
	}
	return ls.idsByAttr(name, val)
}

// ReadErr returns the first page-read error observed while serving this
// snapshot. A nil page from a failed read is also recorded here so a query
// does not silently drop records.
func (mv *matrixView) ReadErr() error {
	for _, ls := range mv.layers {
		if err := ls.loadErr(); err != nil {
			return err
		}
	}
	return nil
}

// Count returns the number of live records in a layer.
func (mv *matrixView) Count(layer int) int {
	if ls := mv.layers[layer]; ls != nil {
		return ls.count
	}
	return 0
}

// editView is a writable copy-on-write matrix. The first mutation of a page
// inside a view clones it, so concurrent readers holding older snapshots are
// never affected and never blocked.
type editView struct {
	*matrixView
	cloned map[int]map[uint64]bool // layer -> page index already cloned in this view
	err    error                   // sticky page-read failure; Commit refuses to publish
}

func newEditView(hdr *Header, layers map[int]*layerState) *editView {
	return &editView{
		matrixView: &matrixView{hdr: hdr, layers: layers},
		cloned:     map[int]map[uint64]bool{},
	}
}

func (e *editView) markCloned(layer int, idx uint64) {
	m := e.cloned[layer]
	if m == nil {
		m = map[uint64]bool{}
		e.cloned[layer] = m
	}
	m[idx] = true
}

// layer returns the (possibly newly created) state for a height. In a
// write transaction this is the transaction's own overlay.
func (e *editView) layer(h int) *layerState {
	ls := e.layers[h]
	if ls == nil {
		ls = newLayerState()
		if def, ok := e.hdr.Layers[h]; ok {
			ls.bindSchema(def)
		}
		e.layers[h] = ls
	}
	return ls
}

// pageForWrite returns the page owning slot idx's page window, cloning it
// first when it is still shared with an older snapshot.
func (e *editView) pageForWrite(layer int, idx uint64) *Page {
	ls := e.layer(layer)
	if e.err != nil {
		return NewPage(layer, idx*SlotsPerPage)
	}
	if e.cloned[layer][idx] {
		return ls.pages[idx]
	}
	var p *Page
	if base := ls.page(idx); base != nil {
		p = base.Clone()
	} else if ls.onDisk(idx) {
		e.err = ls.loadErr()
		if e.err == nil {
			e.err = fmt.Errorf("engine: page %d unreadable", idx)
		}
		return NewPage(layer, idx*SlotsPerPage)
	} else {
		p = NewPage(layer, idx*SlotsPerPage)
	}
	ls.pages[idx] = p
	e.markCloned(layer, idx)
	return p
}

// put installs a record at its own slot and indexes it by ID.
func (e *editView) put(layer int, rec *Record) {
	ls := e.layer(layer)
	p := e.pageForWrite(layer, rec.Slot/SlotsPerPage)
	if e.err != nil {
		return
	}
	_ = p.Put(rec)
	if _, ok := ls.slotOf(rec.ID); !ok {
		ls.count++
	}
	ls.idIndex[rec.ID] = rec.Slot
	if rec.Slot >= ls.nextSlot {
		ls.nextSlot = rec.Slot + 1
	}
}

// remove invalidates a single slot and drops the ID index entry.
func (e *editView) remove(layer int, id uint64) bool {
	ls := e.layers[layer]
	if ls == nil {
		return false
	}
	x, ok := ls.slotOf(id)
	if !ok {
		return false
	}
	p := e.pageForWrite(layer, x/SlotsPerPage)
	if e.err != nil {
		return false
	}
	p.Invalidate(x)
	ls.dropID(id)
	return true
}

// invalidateRange flips validity bits for [start, start+count) across page
// boundaries with word-parallel bitmask operations.
func (e *editView) invalidateRange(layer int, start, count uint64) {
	for count > 0 {
		idx := start / SlotsPerPage
		pageEnd := (idx + 1) * SlotsPerPage
		n := pageEnd - start
		if n > count {
			n = count
		}
		p := e.pageForWrite(layer, idx)
		if e.err != nil {
			return
		}
		p.InvalidateRange(start, n)
		start += n
		count -= n
	}
}

// recordForWrite clones a record and installs the clone, so the record
// remains immutable for readers holding older snapshots.
func (e *editView) recordForWrite(layer int, id uint64) *Record {
	ls := e.layers[layer]
	if ls == nil {
		return nil
	}
	rec := ls.record(id)
	if rec == nil {
		return nil
	}
	c := rec.Clone()
	e.put(layer, c)
	if e.err != nil {
		return nil
	}
	return c
}

func (e *editView) indexInsert(layer int, rec *Record) {
	def, ok := e.hdr.Layers[layer]
	if !ok {
		return
	}
	e.layer(layer).indexInsert(def, rec)
}

func (e *editView) indexRemove(layer int, rec *Record) {
	def, ok := e.hdr.Layers[layer]
	if !ok {
		return
	}
	ls := e.layers[layer]
	if ls == nil {
		return
	}
	ls.indexRemove(def, rec)
}

// Snapshot is an immutable point-in-time view of one branch. Reads through a
// snapshot never block and are unaffected by concurrent writers.
type Snapshot struct {
	Branch string
	Commit uint64 // head commit of the branch when the snapshot was taken
	matrixView
}

// dbState is the atomically published map of branch heads.
type dbState struct {
	branches map[string]*Snapshot
}

func cloneRefs(in map[string]*Snapshot) map[string]*Snapshot {
	out := make(map[string]*Snapshot, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
} // journalCheckpointBytes bounds the append-only delta journal: once the
// journal grows past this size (measured from the last checkpoint), the
// next commit folds it into the canonical page region. Keeps CLI-style
// writes O(write) while bounding replay time on open. The threshold is a
// trade-off: smaller values shorten replay but checkpoint a growing file
// more often during bulk loads (measured ~15% load overhead at 1 MiB,
// ~5% at 8 MiB on a 211k-record load).
const journalCheckpointBytes = 8 << 20 // 8 MiB

// DB is an open BasalDB container (.bsl file). Readers take MVCC snapshots;
// writers append journal records and publish new snapshots atomically.
type DB struct {
	path          string
	hdr           *Header
	f             *os.File
	fsync         bool
	state         atomic.Pointer[dbState]
	seq           atomic.Uint64 // record ID sequence (global, monotonically increasing)
	journalOff    uint64
	ckptThreshold uint64 // journal bytes that trigger an automatic checkpoint

	wmu sync.Mutex // serializes writers: commits, branch events, checkpoints

	gen   *fileGen
	cache *pageCache
	epoch atomic.Uint64 // bumped when the schema gains an index; in-flight txs conflict

	mu         sync.RWMutex
	nextCommit uint64
	commits    map[uint64]*Commit
	forks      map[string]string // branch -> "source@commit"
	events     []LogEntry        // chronological audit trail
	closed     bool

	release func() // drops the cross-process file lock
}

// Path returns the .bsl file path.
func (db *DB) Path() string { return db.path }

// Header returns the parsed header block (treat as read-only).
func (db *DB) Header() *Header { return db.hdr }

// Init creates a new .bsl database with the given layers (heights must be
// contiguous starting at 0) and writes the mandatory initialization commit.
func Init(path string, layers []Layer, meta TxMeta) (*DB, error) {
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if err := validateLayers(layers); err != nil {
		return nil, err
	}
	release, err := lockPath(path+".lock", DefaultLockTimeout)
	if err != nil {
		return nil, err
	}
	failLock := func(err error) (*DB, error) {
		release()
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return failLock(fmt.Errorf("engine: %s already exists", path))
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) {
		f.Close()
		os.Remove(path)
		release()
		return nil, err
	}

	byHeight := make(map[int]Layer, len(layers))
	names := make([]string, 0, len(layers))
	for _, l := range layers {
		byHeight[l.Height] = l
		names = append(names, l.Name)
	}
	hdr := &Header{
		Version:        CurrentVersion,
		Format:         FormatMatrixSlottedPage,
		Layers:         byHeight,
		Branches:       []string{DefaultBranch},
		CommitLogStart: HeaderBlockSize,
		CommitLogEnd:   HeaderBlockSize,
		DataStart:      HeaderBlockSize,
		DataEnd:        HeaderBlockSize,
		IndexStart:     HeaderBlockSize,
		IndexEnd:       HeaderBlockSize,
		JournalStart:   HeaderBlockSize,
	}
	block, err := hdr.Marshal()
	if err != nil {
		return fail(err)
	}
	if _, err := f.WriteAt(block, 0); err != nil {
		return fail(err)
	}

	c := &Commit{
		ID:        1,
		Branch:    DefaultBranch,
		Author:    meta.Author,
		Summary:   meta.Summary,
		Reason:    meta.Reason,
		Timestamp: time.Now().Unix(),
		Ops:       []string{"INIT layers " + strings.Join(names, ",")},
	}
	if err := c.Validate(); err != nil {
		return fail(err)
	}
	jrec, err := marshalJournalCommit(c, nil)
	if err != nil {
		return fail(err)
	}
	if _, err := f.WriteAt(jrec, HeaderBlockSize); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}

	layerStates := make(map[int]*layerState, len(layers))
	for _, l := range layers {
		ls := newLayerState()
		ls.bindSchema(l)
		layerStates[l.Height] = ls
	}
	snap := &Snapshot{Branch: DefaultBranch, Commit: 1, matrixView: matrixView{hdr: hdr, layers: layerStates}}

	db := &DB{
		path:          path,
		hdr:           hdr,
		f:             f,
		journalOff:    HeaderBlockSize + uint64(len(jrec)),
		ckptThreshold: journalCheckpointBytes,
		cache:         newPageCache(DefaultCacheBytes),
		commits:       map[uint64]*Commit{1: c},
		forks:         map[string]string{DefaultBranch: ""},
		events:        []LogEntry{{Commit: c}},
		nextCommit:    2,
		release:       release,
	}
	db.state.Store(&dbState{branches: map[string]*Snapshot{DefaultBranch: snap}})
	return db, nil
}

func validateLayers(layers []Layer) error {
	if len(layers) == 0 {
		return errors.New("engine: at least one layer is required")
	}
	seen := map[string]bool{}
	sorted := append([]Layer(nil), layers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Height < sorted[j].Height })
	for i, l := range sorted {
		if l.Height != i {
			return fmt.Errorf("engine: layer heights must be contiguous from 0 (got height %d at position %d)", l.Height, i)
		}
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("engine: layer %d has an empty name", l.Height)
		}
		if seen[l.Name] {
			return fmt.Errorf("engine: duplicate layer name %q", l.Name)
		}
		seen[l.Name] = true
	}
	return nil
}

// Open opens (and recovers) an existing .bsl database: it parses the ASCII
// header, loads the commit log and the page directory, then replays the
// delta journal. Id pairs stay in a mapped index and are not copied into a
// resident map. Another process that already holds the file is waited on
// for DefaultLockTimeout; past that, Open returns ErrLocked.
func Open(path string) (*DB, error) {
	return OpenTimeout(path, DefaultLockTimeout)
}

// OpenTimeout is Open with an explicit wait for the cross-process lock.
func OpenTimeout(path string, wait time.Duration) (*DB, error) {
	release, err := lockPath(path+".lock", wait)
	if err != nil {
		return nil, err
	}
	db, err := openUnlocked(path)
	if err != nil {
		release()
		return nil, err
	}
	db.release = release
	return db, nil
}

func openUnlocked(path string) (*DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if size < HeaderBlockSize {
		f.Close()
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrCorruptHeader, size)
	}
	block := make([]byte, HeaderBlockSize)
	if _, err := f.ReadAt(block, 0); err != nil {
		f.Close()
		return nil, err
	}
	hdr, err := ParseHeader(block)
	if err != nil {
		f.Close()
		return nil, err
	}
	if hdr.JournalStart > uint64(size) {
		f.Close()
		return nil, fmt.Errorf("%w: journal start %d beyond EOF %d", ErrCorruptHeader, hdr.JournalStart, size)
	}

	logBytes, err := readRegion(f, hdr.CommitLogStart, hdr.CommitLogEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	journalBytes, err := readRegion(f, hdr.JournalStart, uint64(size))
	if err != nil {
		f.Close()
		return nil, err
	}

	entries, err := ParseLogRegion(logBytes)
	if err != nil {
		f.Close()
		return nil, err
	}
	gen := &fileGen{f: f, wide: hdr.Version != VersionV10, cache: newPageCache(DefaultCacheBytes)}
	if err := gen.mapFile(size); err != nil {
		f.Close()
		return nil, err
	}
	var states map[string]map[int]*layerState
	var maxID uint64
	if hdr.IndexEnd > hdr.IndexStart {
		if gen.mapped == nil || hdr.IndexEnd > uint64(len(gen.mapped)) {
			gen.close()
			return nil, fmt.Errorf("%w: index region past EOF", ErrCorruptHeader)
		}
		states, maxID, err = decodeIndex(gen.mapped[hdr.IndexStart:hdr.IndexEnd], gen)
		if err != nil {
			gen.close()
			return nil, err
		}
	} else {
		states, maxID, err = scanDataPages(f, hdr, gen)
		if err != nil {
			gen.close()
			return nil, err
		}
	}
	jrecs, err := parseJournal(journalBytes)
	if err != nil {
		gen.close()
		return nil, err
	}

	commits := map[uint64]*Commit{}
	forks := map[string]string{DefaultBranch: ""}
	heads := map[string]uint64{}
	views := map[string]*editView{}
	ensureState := func(branch string) *editView {
		if v := views[branch]; v != nil {
			return v
		}
		ls := states[branch]
		if ls == nil {
			ls = map[int]*layerState{}
			states[branch] = ls
		}
		v := newEditView(hdr, ls)
		views[branch] = v
		return v
	}
	ensureState(DefaultBranch)

	// Phase 1: commit log region gives the audit history and branch heads
	// as of the last checkpoint (page blocks already hold those states).
	allEvents := append([]LogEntry(nil), entries...)
	for _, e := range entries {
		if e.Commit != nil {
			c := e.Commit
			if _, dup := commits[c.ID]; dup {
				gen.close()
				return nil, fmt.Errorf("engine: duplicate commit %d in log region", c.ID)
			}
			commits[c.ID] = c
			ensureState(c.Branch)
			heads[c.Branch] = c.ID
		} else if e.Branch != nil {
			ev := e.Branch
			if _, dup := forks[ev.Name]; dup && ev.Name != DefaultBranch {
				gen.close()
				return nil, fmt.Errorf("engine: duplicate branch %q in log region", ev.Name)
			}
			forks[ev.Name] = ev.Fork
			ensureState(ev.Name)
			if src, ref := splitFork(ev.Fork); ref != 0 {
				heads[ev.Name] = ref
				_ = src
			}
		}
	}

	// Phase 2: replay the delta journal on top of the checkpointed pages.
	for _, jr := range jrecs {
		if jr.branch != nil {
			ev := jr.branch
			srcName, srcCommit := splitFork(ev.Fork)
			srcView := ensureState(srcName)
			if _, ok := forks[ev.Name]; !ok && ev.Name != DefaultBranch {
				// Fork the source state as of its current replayed
				// state. The source view keeps mutating its state
				// in place, so the fork needs a real deep copy.
				cloned := make(map[int]*layerState, len(srcView.layers))
				for h, ls := range srcView.layers {
					cloned[h] = ls.copyRoot()
				}
				views[ev.Name] = newEditView(hdr, cloned)
				states[ev.Name] = cloned
			}
			forks[ev.Name] = ev.Fork
			heads[ev.Name] = srcCommit
			allEvents = append(allEvents, LogEntry{Branch: ev})
			continue
		}
		c := jr.commit
		if _, dup := commits[c.ID]; dup {
			gen.close()
			return nil, fmt.Errorf("engine: duplicate commit %d in journal", c.ID)
		}
		commits[c.ID] = c
		view := ensureState(c.Branch)
		for _, op := range jr.ops {
			if err := applyOp(view, op); err != nil {
				gen.close()
				return nil, err
			}
		}
		for _, op := range jr.ops {
			if op.Record != nil && op.Record.ID > maxID {
				maxID = op.Record.ID
			}
		}
		heads[c.Branch] = c.ID
		allEvents = append(allEvents, LogEntry{Commit: c})
	}

	snaps := make(map[string]*Snapshot, len(states))
	for branch, ls := range states {
		snaps[branch] = &Snapshot{Branch: branch, Commit: heads[branch], matrixView: matrixView{hdr: hdr, layers: ls}}
	}

	db := &DB{
		path:          path,
		hdr:           hdr,
		f:             f,
		gen:           gen,
		cache:         gen.cache,
		journalOff:    uint64(size),
		ckptThreshold: journalCheckpointBytes,
		commits:       commits,
		forks:         forks,
		events:        allEvents,
		nextCommit:    1,
	}
	for id := range commits {
		if id >= db.nextCommit {
			db.nextCommit = id + 1
		}
	}
	db.seq.Store(maxID)
	db.state.Store(&dbState{branches: snaps})
	return db, nil
}

func readRegion(f *os.File, start, end uint64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("%w: inverted region %d-%d", ErrCorruptHeader, start, end)
	}
	n := end - start
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, int64(start)); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// applyOp replays one journal mutation into a branch's edit view. Replaying
// the same journal always reproduces the same matrix state (cascade deletes
// re-run the same bitmask invalidation on the same child pointer arrays).
func applyOp(view *editView, op Op) error {
	switch op.Kind {
	case opCreate, opUpdate:
		if op.Record == nil {
			return fmt.Errorf("engine: journal op %d carries no record", op.Kind)
		}
		rec := op.Record.Clone()
		if op.Kind == opUpdate {
			if ls := view.layers[op.Layer]; ls != nil {
				if old := ls.record(rec.ID); old != nil {
					view.indexRemove(op.Layer, old)
				}
			}
		}
		view.put(op.Layer, rec)
		if view.err != nil {
			return view.err
		}
		view.indexInsert(op.Layer, rec)
		return nil
	case opDelete:
		_, err := cascadeDelete(view, op.Layer, op.ID)
		return err
	default:
		return fmt.Errorf("engine: unknown journal op kind %d", op.Kind)
	}
}

// SetSync toggles fsync-on-commit durability.
func (db *DB) SetSync(v bool) {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	db.fsync = v
}

// Snapshot returns the current immutable snapshot of a branch. Taking a
// snapshot never blocks writers, and reads through it never observe
// concurrent writes (snapshot isolation).
func (db *DB) Snapshot(branch string) (*Snapshot, error) {
	if branch == "" {
		branch = DefaultBranch
	}
	s := db.state.Load().branches[branch]
	if s == nil {
		return nil, fmt.Errorf("engine: unknown branch %q", branch)
	}
	return s, nil
}

// WriteTx is a write transaction. Mutations are applied to a private
// copy-on-write view of the branch; Commit appends one journal record and
// atomically publishes a new snapshot, so reads and writes proceed at the
// same time without blocking each other.
type WriteTx struct {
	db     *DB
	branch string
	meta   TxMeta
	base   *Snapshot
	view   *editView
	ops    []Op
	done   bool
	epoch  uint64
}

// Begin starts a write transaction on top of the branch head snapshot.
// Record writes do not require audit metadata; whatever TxMeta carries
// stamps the resulting commit block.
func (db *DB) Begin(branch string, meta TxMeta) (*WriteTx, error) {
	if branch == "" {
		branch = DefaultBranch
	}
	base, err := db.Snapshot(branch)
	if err != nil {
		return nil, err
	}
	// O(1) regardless of database size: the transaction gets an empty
	// copy-on-write overlay per layer, sharing the published state as its
	// base (issue #34).
	layers := make(map[int]*layerState, len(base.layers))
	for h, ls := range base.layers {
		layers[h] = ls.overlay()
	}
	return &WriteTx{
		db:     db,
		branch: branch,
		meta:   meta,
		base:   base,
		view:   newEditView(db.hdr, layers),
		epoch:  db.epoch.Load(),
	}, nil
}

// Branch returns the branch the transaction writes to.
func (tx *WriteTx) Branch() string { return tx.branch }

// Base returns the snapshot the transaction started from.
func (tx *WriteTx) Base() *Snapshot { return tx.base }

func (tx *WriteTx) check() error {
	if tx.done {
		return ErrTxDone
	}
	return nil
}

// validateAttrSizes rejects an attribute list that cannot be stored in a
// v1.1 tuple. The check happens before any mutation so a too-large value
// never reaches the journal.
func validateAttrSizes(layer Layer, attrs []string) error {
	if uint64(len(attrs)) > math.MaxUint32 {
		return fmt.Errorf("engine: record has %d attributes, max %d", len(attrs), uint64(math.MaxUint32))
	}
	for i, a := range attrs {
		if uint64(len(a)) > math.MaxUint32 {
			name := strconv.Itoa(i)
			if i < len(layer.Attrs) {
				name = layer.Attrs[i]
			}
			return fmt.Errorf("engine: attribute %q is %d bytes, max %d", name, len(a), uint64(math.MaxUint32))
		}
	}
	return nil
}

// Create allocates a new record in the given layer, optionally under a
// parent record of the layer above (parentID 0 = matrix root). IDs and
// contiguous X slots are assigned by the engine.
func (tx *WriteTx) Create(layerName string, parentID uint64, attrs []string) (*Record, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, err
	}
	if parentID != 0 && height == 0 {
		return nil, errors.New("engine: records of the root layer cannot have a parent")
	}
	layer := tx.db.hdr.Layers[height]
	if err := validateAttrSizes(layer, attrs); err != nil {
		return nil, err
	}
	if parentID != 0 {
		parent, ok := tx.view.Record(height-1, parentID)
		if !ok {
			return nil, fmt.Errorf("engine: parent %d not found in layer %q", parentID, tx.db.hdr.layerName(height-1))
		}
		if uint64(len(parent.Children)) >= math.MaxUint32 {
			return nil, fmt.Errorf("engine: record %d already has %d children, max %d", parentID, len(parent.Children), uint64(math.MaxUint32))
		}
		p := tx.view.recordForWrite(height-1, parentID)
		if p == nil {
			return nil, fmt.Errorf("engine: parent %d disappeared", parentID)
		}
		p.Children = append(p.Children, 0) // placeholder, filled below
		_ = parent
	}

	ls := tx.view.layer(height)
	rec := &Record{
		ID:     tx.db.seq.Add(1),
		Slot:   ls.nextSlot,
		Parent: parentID,
		Attrs:  append([]string(nil), attrs...),
	}
	ls.nextSlot++
	tx.view.put(height, rec)
	if tx.view.err != nil {
		return nil, tx.view.err
	}
	tx.view.indexInsert(height, rec)

	if parentID != 0 {
		p := tx.view.recordForWrite(height-1, parentID)
		p.Children[len(p.Children)-1] = rec.ID
	}
	tx.ops = append(tx.ops, Op{Kind: opCreate, Layer: height, Record: rec.Clone()})
	return rec.Clone(), nil
}

// Update replaces the scalar attributes of an existing record.
func (tx *WriteTx) Update(layerName string, id uint64, attrs []string) (*Record, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, err
	}
	old, ok := tx.view.Record(height, id)
	if !ok {
		return nil, fmt.Errorf("engine: record %d not found in layer %q", id, layerName)
	}
	if err := validateAttrSizes(tx.db.hdr.Layers[height], attrs); err != nil {
		return nil, err
	}
	prev := old.Clone()
	rec := tx.view.recordForWrite(height, id)
	if tx.view.err != nil {
		return nil, tx.view.err
	}
	if rec == nil {
		return nil, fmt.Errorf("engine: record %d not found in layer %q", id, layerName)
	}
	tx.view.indexRemove(height, prev)
	rec.Attrs = append([]string(nil), attrs...)
	tx.view.indexInsert(height, rec)
	tx.ops = append(tx.ops, Op{Kind: opUpdate, Layer: height, Record: rec.Clone()})
	return rec.Clone(), nil
}

// Record reads a record inside the transaction (read-your-writes).
func (tx *WriteTx) Record(layerName string, id uint64) (*Record, bool) {
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, false
	}
	return tx.view.Record(height, id)
}

// RecordBySlot reads a record by X slot inside the transaction.
func (tx *WriteTx) RecordBySlot(layerName string, x uint64) (*Record, bool) {
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, false
	}
	return tx.view.RecordBySlot(height, x)
}

// Records lists the records of a layer inside the transaction.
func (tx *WriteTx) Records(layerName string) ([]*Record, error) {
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, err
	}
	return tx.view.Records(height), nil
}

// Layers returns the layer definitions (used for name validation).
func (tx *WriteTx) Layers() []Layer {
	return tx.view.Layers()
}

// Children lists the child records of a node inside the transaction.
func (tx *WriteTx) Children(layerName string, id uint64) ([]*Record, error) {
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, err
	}
	return tx.view.Children(height, id), nil
}

// IDsByAttr resolves an indexed equality inside the transaction, including
// this transaction's own writes.
func (tx *WriteTx) IDsByAttr(layerName, field, val string) ([]uint64, bool) {
	height, err := tx.view.height(layerName)
	if err != nil {
		return nil, false
	}
	return tx.view.IDsByAttr(height, field, val)
}

// Commit appends the commit block to the journal and atomically publishes
// the new branch head. Concurrent readers keep their snapshots; concurrent
// writers on other branches proceed. If the branch head moved since Begin,
// Commit fails with ErrConflict instead of silently losing the update.
func (tx *WriteTx) Commit() (*Snapshot, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	db := tx.db
	db.wmu.Lock()
	defer db.wmu.Unlock()

	db.mu.RLock()
	closed := db.closed
	db.mu.RUnlock()
	if closed {
		return nil, errors.New("engine: database is closed")
	}

	cur := db.state.Load().branches[tx.branch]
	if cur == nil {
		return nil, fmt.Errorf("engine: unknown branch %q", tx.branch)
	}
	if cur.Commit != tx.base.Commit || tx.epoch != db.epoch.Load() {
		return nil, fmt.Errorf("%w: branch %q head moved from commit %d to %d",
			ErrConflict, tx.branch, tx.base.Commit, cur.Commit)
	}
	if tx.view.err != nil {
		return nil, tx.view.err
	}

	db.mu.Lock()
	id := db.nextCommit
	db.nextCommit++
	db.mu.Unlock()

	c := &Commit{
		ID:        id,
		Branch:    tx.branch,
		Parent:    tx.base.Commit,
		Author:    tx.meta.Author,
		Summary:   tx.meta.Summary,
		Reason:    tx.meta.Reason,
		Timestamp: time.Now().Unix(),
		Ops:       tx.opTexts(),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Also reject a parent whose child list no longer fits on disk. Create
	// checks before each append; this is the backstop for anything else
	// that mutated a record before the journal write.
	for _, op := range tx.ops {
		if op.Record == nil {
			continue
		}
		if err := recordFits(op.Record, true); err != nil {
			return nil, err
		}
	}
	jrec, err := marshalJournalCommit(c, tx.ops)
	if err != nil {
		return nil, err
	}
	if err := db.appendJournal(jrec); err != nil {
		return nil, err
	}

	// Publish the transaction's overlays while they are shallow and
	// sparse; flatten them into a root once they are deep or dense so the
	// copy work stays amortized O(1) per commit and reads walk a bounded
	// chain.
	for h, ls := range tx.view.layers {
		tx.view.layers[h] = ls.settled()
	}

	snap := &Snapshot{
		Branch:     tx.branch,
		Commit:     c.ID,
		matrixView: matrixView{hdr: db.hdr, layers: tx.view.layers},
	}
	next := cloneRefs(db.state.Load().branches)
	next[tx.branch] = snap
	db.state.Store(&dbState{branches: next})

	db.mu.Lock()
	db.commits[c.ID] = c
	db.events = append(db.events, LogEntry{Commit: c})
	db.mu.Unlock()

	// The commit is durable in the journal; fold it into the page region
	// once the journal passes the size threshold so replay on open stays
	// bounded. A failed checkpoint here is not fatal to the commit (the
	// journal record is already written) and is retried by the next
	// commit or by Close.
	if db.ckptThreshold > 0 && db.journalOff > db.hdr.JournalStart &&
		db.journalOff-db.hdr.JournalStart >= db.ckptThreshold {
		_ = db.checkpointLocked()
	}

	tx.done = true
	return snap, nil
}

// Rollback discards the transaction without touching the database.
func (tx *WriteTx) Rollback() {
	tx.done = true
}

func (tx *WriteTx) opTexts() []string {
	out := make([]string, 0, len(tx.ops))
	for _, op := range tx.ops {
		name := tx.db.hdr.layerName(op.Layer)
		switch op.Kind {
		case opCreate:
			out = append(out, fmt.Sprintf("CREATE %s#%d id=%d parent=%d", name, op.Record.Slot, op.Record.ID, op.Record.Parent))
		case opUpdate:
			out = append(out, fmt.Sprintf("UPDATE %s#%d id=%d", name, op.Record.Slot, op.Record.ID))
		case opDelete:
			out = append(out, fmt.Sprintf("DELETE %s id=%d cascade", name, op.ID))
		}
	}
	return out
}

// appendJournal writes one journal record at the end of the file.
func (db *DB) appendJournal(rec []byte) error {
	if _, err := db.f.WriteAt(rec, int64(db.journalOff)); err != nil {
		return err
	}
	db.journalOff += uint64(len(rec))
	if db.fsync {
		return db.f.Sync()
	}
	return nil
}

// Checkpoint compacts the container: the whole state (commit log region and
// slotted data pages of every branch) is rewritten into the canonical .bsl
// layout and the delta journal restarts empty. Readers keep their snapshots
// while the rewrite happens.
func (db *DB) Checkpoint() error {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	return db.checkpointLocked()
}

func (db *DB) checkpointLocked() error {
	db.mu.RLock()
	closed := db.closed
	logText := LogRegionText(db.events)
	branchNames := make([]string, 0, len(db.forks))
	for name := range db.forks {
		branchNames = append(branchNames, name)
	}
	db.mu.RUnlock()
	if closed {
		return errors.New("engine: database is closed")
	}
	sort.Strings(branchNames)

	st := db.state.Load()
	logBytes := []byte(logText)
	tmp := db.path + ".tmp"
	os.Remove(tmp)
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := tf.Write(make([]byte, HeaderBlockSize)); err != nil {
		return fail(err)
	}
	if _, err := tf.Write(logBytes); err != nil {
		return fail(err)
	}
	off := uint64(HeaderBlockSize + len(logBytes))
	metas := map[string]map[int]*layerMeta{}
	// Unchanged pages are shared across branches. The key is the in-memory
	// page pointer (copy-on-write) or the source file offset (already on disk).
	type shareKey struct {
		page *Page
		gen  *fileGen
		off  uint64
	}
	written := map[shareKey]pageLoc{}
	var blockLen [4]byte
	var nameLen [2]byte
	for _, branch := range branchNames {
		snap := st.branches[branch]
		if snap == nil {
			continue
		}
		for _, height := range snap.hdr.LayerHeights() {
			ls := snap.layers[height]
			if ls == nil {
				continue
			}
			lm := exportLayer(ls, height)
			idxs := ls.pageIndexes()
			sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
			for _, idx := range idxs {
				p, srcGen, srcLoc, ok := pageSource(ls, idx)
				if !ok {
					if err := ls.loadErr(); err != nil {
						return fail(err)
					}
					return fail(fmt.Errorf("engine: missing page %d while checkpointing %s", idx, branch))
				}
				key := shareKey{page: p, gen: srcGen, off: srcLoc.off}
				if prev, ok := written[key]; ok {
					lm.refs[idx] = prev
					continue
				}
				var payload []byte
				if p != nil {
					pageBytes, err := p.MarshalBinary()
					if err != nil {
						return fail(err)
					}
					payload = make([]byte, 0, 2+len(branch)+len(pageBytes))
					binary.LittleEndian.PutUint16(nameLen[:], uint16(len(branch)))
					payload = append(payload, nameLen[:]...)
					payload = append(payload, branch...)
					payload = append(payload, pageBytes...)
				} else {
					raw := make([]byte, srcLoc.n)
					if srcGen == nil || srcGen.f == nil {
						return fail(fmt.Errorf("engine: page %d has no source file", idx))
					}
					if _, err := srcGen.f.ReadAt(raw, int64(srcLoc.off)+4); err != nil {
						return fail(err)
					}
					payload = raw
				}
				if len(payload) > int(^uint32(0)) {
					return fail(fmt.Errorf("engine: page block exceeds uint32"))
				}
				binary.LittleEndian.PutUint32(blockLen[:], uint32(len(payload)))
				if _, err := tf.Write(blockLen[:]); err != nil {
					return fail(err)
				}
				if _, err := tf.Write(payload); err != nil {
					return fail(err)
				}
				loc := pageLoc{off: off, n: uint32(len(payload))}
				lm.refs[idx] = loc
				written[key] = loc
				off += 4 + uint64(len(payload))
			}
			if metas[branch] == nil {
				metas[branch] = map[int]*layerMeta{}
			}
			metas[branch][height] = lm
		}
	}
	dataEnd := off
	indexN, err := writeIndex(tf, branchNames, metas)
	if err != nil {
		return fail(err)
	}
	indexEnd := dataEnd + uint64(indexN)

	hdr := *db.hdr
	// A checkpoint rewrites every tuple in the wide layout, so the file
	// becomes 1.1 even when it was opened as 1.0.
	hdr.Version = CurrentVersion
	hdr.Branches = branchNames
	hdr.CommitLogStart = HeaderBlockSize
	hdr.CommitLogEnd = HeaderBlockSize + uint64(len(logBytes))
	hdr.DataStart = hdr.CommitLogEnd
	hdr.DataEnd = dataEnd
	hdr.IndexStart = dataEnd
	hdr.IndexEnd = indexEnd
	hdr.JournalStart = indexEnd
	block, err := hdr.Marshal()
	if err != nil {
		return fail(err)
	}
	if _, err := tf.WriteAt(block, 0); err != nil {
		return fail(err)
	}
	if err := tf.Sync(); err != nil {
		return fail(err)
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, db.path); err != nil {
		os.Remove(tmp)
		return err
	}
	nf, err := os.OpenFile(db.path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	newGen := &fileGen{f: nf, wide: true, cache: db.cache}
	oldGen := db.gen
	oldFile := db.f
	if db.cache == nil {
		db.cache = newPageCache(DefaultCacheBytes)
		newGen.cache = db.cache
	}
	stSize, err := nf.Stat()
	if err != nil {
		nf.Close()
		return err
	}
	if err := newGen.mapFile(stSize.Size()); err != nil {
		nf.Close()
		return err
	}
	if oldGen != nil {
		db.cache.dropGen(oldGen)
		// The previous inode stays readable for snapshots that still point
		// at it. The finalizer closes that fd once those snapshots are gone.
		runtime.SetFinalizer(oldGen, func(g *fileGen) { g.close() })
	} else if oldFile != nil {
		oldFile.Close()
	}
	db.gen = newGen
	db.f = nf
	hdr.Layers = cloneLayers(hdr.Layers)
	db.hdr = &hdr
	db.journalOff = hdr.JournalStart

	var states map[string]map[int]*layerState
	if hdr.IndexEnd > hdr.IndexStart {
		if newGen.mapped == nil || hdr.IndexEnd > uint64(len(newGen.mapped)) {
			return fmt.Errorf("%w: index region past EOF after checkpoint", ErrCorruptHeader)
		}
		states, _, err = decodeIndex(newGen.mapped[hdr.IndexStart:hdr.IndexEnd], newGen)
		if err != nil {
			return err
		}
	} else {
		states = map[string]map[int]*layerState{}
	}
	next := make(map[string]*Snapshot, len(st.branches))
	for _, branch := range branchNames {
		old := st.branches[branch]
		if old == nil {
			continue
		}
		layers := states[branch]
		if layers == nil {
			layers = map[int]*layerState{}
		}
		next[branch] = &Snapshot{
			Branch:     branch,
			Commit:     old.Commit,
			matrixView: matrixView{hdr: db.hdr, layers: layers},
		}
	}
	db.state.Store(&dbState{branches: next})
	return nil
}

// pageSource reports where a page's bytes live without decoding a disk page.
// An in-memory page is returned as p. A disk page is returned as gen+loc.
func pageSource(l *layerState, idx uint64) (*Page, *fileGen, pageLoc, bool) {
	for cur := l; cur != nil; cur = cur.base {
		if p := cur.pages[idx]; p != nil {
			return p, nil, pageLoc{}, true
		}
	}
	for cur := l; cur != nil; cur = cur.base {
		if cur.disk == nil || cur.disk.gen == nil {
			continue
		}
		if loc, ok := cur.disk.refs[idx]; ok {
			return nil, cur.disk.gen, loc, true
		}
	}
	return nil, nil, pageLoc{}, false
}

// exportLayer records the live layer so the index writer can stream ids.
// It does not copy the id table into a map.
func exportLayer(l *layerState, height int) *layerMeta {
	return &layerMeta{
		height:   height,
		nextSlot: l.nextSlot,
		count:    l.count,
		refs:     map[uint64]pageLoc{},
		src:      l,
		indexes:  flattenIndexMap(l),
	}
}

func cloneLayers(in map[int]Layer) map[int]Layer {
	out := make(map[int]Layer, len(in))
	for k, v := range in {
		v.Attrs = append([]string(nil), v.Attrs...)
		v.Indexed = append([]string(nil), v.Indexed...)
		out[k] = v
	}
	return out
}

// abandon closes the file and the lock without checkpointing. Tests use it
// to stand in for a crashed process, which drops its locks when it exits.
func (db *DB) abandon() error {
	db.mu.Lock()
	db.closed = true
	db.mu.Unlock()
	var err error
	if db.gen != nil {
		db.gen.unmap()
		if db.gen.f == db.f {
			db.gen.f = nil
		}
	}
	if db.f != nil {
		err = db.f.Close()
		db.f = nil
	}
	if db.release != nil {
		db.release()
		db.release = nil
	}
	return err
}

// Close checkpoints the database (only if the journal holds records since
// the last checkpoint) and closes the container file. A clean close never
// rewrites the file: read-only workloads leave the container byte-for-byte
// untouched, with no rename and no new inode.
func (db *DB) Close() error {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	db.mu.RLock()
	closed := db.closed
	db.mu.RUnlock()
	if closed {
		return nil
	}
	var err error
	if db.journalOff > db.hdr.JournalStart {
		err = db.checkpointLocked()
	}
	db.mu.Lock()
	db.closed = true
	db.mu.Unlock()
	if db.gen != nil {
		db.gen.unmap()
		if db.gen.f == db.f {
			db.gen.f = nil
		}
	}
	if db.f != nil {
		if cerr := db.f.Close(); err == nil {
			err = cerr
		}
	}
	if db.release != nil {
		db.release()
		db.release = nil
	}
	return err
}

// SetCacheLimit caps decoded slotted pages. Secondary attribute indexes
// stay resident. The id index is mapped from the file and is not part of
// this budget. n <= 0 restores the default budget.
func (db *DB) SetCacheLimit(n int) {
	if db.cache == nil {
		db.cache = newPageCache(n)
		if db.gen != nil {
			db.gen.cache = db.cache
		}
		return
	}
	db.cache.setMax(n)
	if db.gen != nil && db.gen.cache == nil {
		db.gen.cache = db.cache
	}
}

// PageLoads reports how many slotted pages have been decoded from disk
// since this file generation was opened. A checkpoint starts a new count.
func (db *DB) PageLoads() uint64 {
	if db.gen == nil {
		return 0
	}
	return db.gen.loads.Load()
}

// CreateIndex declares a hash index on an existing attribute and checkpoints
// so the index survives reopen. It is idempotent. In-flight write
// transactions conflict and must retry, because the published head changes
// under them without a new commit id.
func (db *DB) CreateIndex(layerName, attr string) error {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	db.mu.RLock()
	closed := db.closed
	db.mu.RUnlock()
	if closed {
		return errors.New("engine: database is closed")
	}
	def, ok := db.hdr.LayerByName(layerName)
	if !ok {
		return fmt.Errorf("engine: unknown layer %q", layerName)
	}
	pos, ok := def.AttrIndex(attr)
	if !ok {
		return fmt.Errorf("engine: layer %q has no attribute %q", layerName, attr)
	}
	canon := def.Attrs[pos]
	if def.IsIndexed(canon) {
		return nil
	}

	layers := cloneLayers(db.hdr.Layers)
	l := layers[def.Height]
	l.Indexed = append(l.Indexed, canon)
	layers[def.Height] = l
	hdr := *db.hdr
	hdr.Layers = layers
	newHdr := &hdr

	st := db.state.Load()
	next := cloneRefs(st.branches)
	for branch, snap := range st.branches {
		copied := make(map[int]*layerState, len(snap.layers)+1)
		for h, ls := range snap.layers {
			copied[h] = ls
		}
		base := copied[def.Height]
		if base == nil {
			base = newLayerState()
		}
		ov := base.overlay()
		ix := newAttrIndex()
		snap.Walk(def.Height, false, func(r *Record) bool {
			if pos < len(r.Attrs) {
				ix.add(r.Attrs[pos], r.ID)
			}
			return true
		})
		if err := snap.ReadErr(); err != nil {
			return err
		}
		if ov.indexes == nil {
			ov.indexes = map[string]*attrIndex{}
		}
		ov.indexes[canon] = ix
		copied[def.Height] = ov
		next[branch] = &Snapshot{
			Branch:     branch,
			Commit:     snap.Commit,
			matrixView: matrixView{hdr: newHdr, layers: copied},
		}
	}
	db.epoch.Add(1)
	db.hdr = newHdr
	db.state.Store(&dbState{branches: next})
	return db.checkpointLocked()
}

// splitFork splits a fork reference "branch@commit".
func splitFork(fork string) (string, uint64) {
	name, refStr, _ := strings.Cut(fork, "@")
	var ref uint64
	fmt.Sscanf(refStr, "%d", &ref)
	return strings.TrimSpace(name), ref
}
