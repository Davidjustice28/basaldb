package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
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
	base     *layerState       // shared immutable parent (nil for a root)
	pages    map[uint64]*Page  // pages created/modified relative to base
	idIndex  map[uint64]uint64 // ids added relative to base
	dels     map[uint64]bool   // ids deleted relative to base (tombstones)
	nextSlot uint64
	count    int // live records, maintained across the overlay chain
}

func newLayerState() *layerState {
	return &layerState{pages: map[uint64]*Page{}, idIndex: map[uint64]uint64{}}
}

// overlay returns a writable, initially empty copy-on-write layer on top of
// l. O(1): the base state is shared, not copied.
func (l *layerState) overlay() *layerState {
	if l == nil {
		return newLayerState()
	}
	return &layerState{
		base:     l,
		pages:    map[uint64]*Page{},
		idIndex:  map[uint64]uint64{},
		nextSlot: l.nextSlot,
		count:    l.count,
	}
}

// page returns the page with the given index, consulting the overlay chain
// top-down (the most recent entry wins).
func (l *layerState) page(idx uint64) *Page {
	for ; l != nil; l = l.base {
		if p := l.pages[idx]; p != nil {
			return p
		}
	}
	return nil
}

// slotOf resolves an id to its slot through the overlay chain.
func (l *layerState) slotOf(id uint64) (uint64, bool) {
	for ; l != nil; l = l.base {
		if x, ok := l.idIndex[id]; ok {
			return x, true
		}
		if l.dels[id] {
			return 0, false
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

// allPages merges the overlay chain into one index -> page map (topmost
// entry wins). The result must be treated as read-only.
func (l *layerState) allPages() map[uint64]*Page {
	if l == nil {
		return nil
	}
	out := map[uint64]*Page{}
	if l.base != nil {
		out = l.base.allPages()
	}
	for idx, p := range l.pages {
		out[idx] = p
	}
	return out
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
	c := &layerState{
		pages:    make(map[uint64]*Page, len(root.pages)+len(l.pages)),
		idIndex:  make(map[uint64]uint64, len(root.idIndex)+len(l.idIndex)),
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

// copyRoot returns a deep copy of a materialized state whose pages may keep
// being mutated in place by an unpublished view (journal replay forks).
func (l *layerState) copyRoot() *layerState {
	m := l.materialize()
	c := &layerState{
		pages:    make(map[uint64]*Page, len(m.pages)),
		idIndex:  make(map[uint64]uint64, len(m.idIndex)),
		nextSlot: m.nextSlot,
		count:    m.count,
	}
	for k, v := range m.pages {
		c.pages[k] = v
	}
	for k, v := range m.idIndex {
		c.idIndex[k] = v
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
	pages := ls.allPages()
	idxs := make([]uint64, 0, len(pages))
	for idx := range pages {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	var out []*Record
	for _, idx := range idxs {
		out = append(out, pages[idx].Slots()...)
	}
	return out
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
		e.layers[h] = ls
	}
	return ls
}

// pageForWrite returns the page owning slot idx's page window, cloning it
// first when it is still shared with an older snapshot.
func (e *editView) pageForWrite(layer int, idx uint64) *Page {
	ls := e.layer(layer)
	if e.cloned[layer][idx] {
		return ls.pages[idx]
	}
	var p *Page
	if base := ls.page(idx); base != nil {
		p = base.Clone()
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
		e.pageForWrite(layer, idx).InvalidateRange(start, n)
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
	return c
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

	mu         sync.RWMutex
	nextCommit uint64
	commits    map[uint64]*Commit
	forks      map[string]string // branch -> "source@commit"
	events     []LogEntry        // chronological audit trail
	closed     bool
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
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("engine: %s already exists", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) {
		f.Close()
		os.Remove(path)
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
	jrec := marshalJournalCommit(c, nil)
	if _, err := f.WriteAt(jrec, HeaderBlockSize); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}

	layerStates := make(map[int]*layerState, len(layers))
	for _, l := range layers {
		layerStates[l.Height] = newLayerState()
	}
	snap := &Snapshot{Branch: DefaultBranch, Commit: 1, matrixView: matrixView{hdr: hdr, layers: layerStates}}

	db := &DB{
		path:          path,
		hdr:           hdr,
		f:             f,
		journalOff:    HeaderBlockSize + uint64(len(jrec)),
		ckptThreshold: journalCheckpointBytes,
		commits:       map[uint64]*Commit{1: c},
		forks:         map[string]string{DefaultBranch: ""},
		events:        []LogEntry{{Commit: c}},
		nextCommit:    2,
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
// header, loads the commit log region and slotted data pages, then replays
// the delta journal so readers see a consistent recovered state.
func Open(path string) (*DB, error) {
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
	dataBytes, err := readRegion(f, hdr.DataStart, hdr.DataEnd)
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
	states, maxID, err := loadPageBlocks(dataBytes)
	if err != nil {
		f.Close()
		return nil, err
	}
	jrecs, err := parseJournal(journalBytes)
	if err != nil {
		f.Close()
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
				f.Close()
				return nil, fmt.Errorf("engine: duplicate commit %d in log region", c.ID)
			}
			commits[c.ID] = c
			ensureState(c.Branch)
			heads[c.Branch] = c.ID
		} else if e.Branch != nil {
			ev := e.Branch
			if _, dup := forks[ev.Name]; dup && ev.Name != DefaultBranch {
				f.Close()
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
			f.Close()
			return nil, fmt.Errorf("engine: duplicate commit %d in journal", c.ID)
		}
		commits[c.ID] = c
		view := ensureState(c.Branch)
		for _, op := range jr.ops {
			if err := applyOp(view, op); err != nil {
				f.Close()
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

// loadPageBlocks decodes the slotted binary data region into per-branch
// layer states: [u32 blockLen][u16 branchLen][branch][page bytes]...
func loadPageBlocks(data []byte) (map[string]map[int]*layerState, uint64, error) {
	states := map[string]map[int]*layerState{}
	var maxID uint64
	off := 0
	for off < len(data) {
		if len(data)-off < 4 {
			return nil, 0, fmt.Errorf("%w: truncated page block header", ErrCorruptPage)
		}
		blockLen := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if blockLen < 2 || len(data)-off < blockLen {
			return nil, 0, fmt.Errorf("%w: truncated page block (%d bytes)", ErrCorruptPage, blockLen)
		}
		block := data[off : off+blockLen]
		off += blockLen
		nameLen := int(binary.LittleEndian.Uint16(block[0:2]))
		if nameLen <= 0 || 2+nameLen > len(block) {
			return nil, 0, fmt.Errorf("%w: bad branch tag in page block", ErrCorruptPage)
		}
		branch := string(block[2 : 2+nameLen])
		page, err := UnmarshalPage(block[2+nameLen:])
		if err != nil {
			return nil, 0, err
		}
		lsMap := states[branch]
		if lsMap == nil {
			lsMap = map[int]*layerState{}
			states[branch] = lsMap
		}
		ls := lsMap[page.Layer]
		if ls == nil {
			ls = newLayerState()
			lsMap[page.Layer] = ls
		}
		ls.pages[page.SlotBase/SlotsPerPage] = page
		for _, rec := range page.Slots() {
			ls.idIndex[rec.ID] = rec.Slot
			if rec.Slot >= ls.nextSlot {
				ls.nextSlot = rec.Slot + 1
			}
			if rec.ID > maxID {
				maxID = rec.ID
			}
			ls.count++
		}
	}
	return states, maxID, nil
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
		view.put(op.Layer, op.Record.Clone())
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
	if parentID != 0 {
		parent, ok := tx.view.Record(height-1, parentID)
		if !ok {
			return nil, fmt.Errorf("engine: parent %d not found in layer %q", parentID, tx.db.hdr.layerName(height-1))
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
	rec := tx.view.recordForWrite(height, id)
	if rec == nil {
		return nil, fmt.Errorf("engine: record %d not found in layer %q", id, layerName)
	}
	rec.Attrs = append([]string(nil), attrs...)
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
	if cur.Commit != tx.base.Commit {
		return nil, fmt.Errorf("%w: branch %q head moved from commit %d to %d",
			ErrConflict, tx.branch, tx.base.Commit, cur.Commit)
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
	if err := db.appendJournal(marshalJournalCommit(c, tx.ops)); err != nil {
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
	var data bytes.Buffer
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
			pages := ls.allPages()
			idxs := make([]uint64, 0, len(pages))
			for idx := range pages {
				idxs = append(idxs, idx)
			}
			sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
			for _, idx := range idxs {
				pageBytes, err := pages[idx].MarshalBinary()
				if err != nil {
					return err
				}
				payload := make([]byte, 0, 2+len(branch)+len(pageBytes))
				binary.LittleEndian.PutUint16(nameLen[:], uint16(len(branch)))
				payload = append(payload, nameLen[:]...)
				payload = append(payload, branch...)
				payload = append(payload, pageBytes...)
				binary.LittleEndian.PutUint32(blockLen[:], uint32(len(payload)))
				data.Write(blockLen[:])
				data.Write(payload)
			}
		}
	}

	logBytes := []byte(logText)
	hdr := *db.hdr
	hdr.Branches = branchNames
	hdr.CommitLogStart = HeaderBlockSize
	hdr.CommitLogEnd = hdr.CommitLogStart + uint64(len(logBytes))
	hdr.DataStart = hdr.CommitLogEnd
	hdr.DataEnd = hdr.DataStart + uint64(data.Len())
	hdr.JournalStart = hdr.DataEnd
	block, err := hdr.Marshal()
	if err != nil {
		return err
	}

	tmp := db.path + ".tmp"
	os.Remove(tmp)
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := tf.WriteAt(block, 0); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := tf.WriteAt(logBytes, int64(hdr.CommitLogStart)); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := tf.WriteAt(data.Bytes(), int64(hdr.DataStart)); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	old := db.f
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
	old.Close()
	db.f = nf
	db.hdr = &hdr
	db.journalOff = hdr.JournalStart
	return nil
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
	if cerr := db.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// splitFork splits a fork reference "branch@commit".
func splitFork(fork string) (string, uint64) {
	name, refStr, _ := strings.Cut(fork, "@")
	var ref uint64
	fmt.Sscanf(refStr, "%d", &ref)
	return strings.TrimSpace(name), ref
}
