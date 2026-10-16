package engine

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// DefaultCacheBytes is the decoded-page cache budget. The id index and
// secondary indexes stay resident; this cap bounds slotted pages only.
const DefaultCacheBytes = 64 << 20

// pageLoc is one slotted page inside a data region: off is the absolute
// file offset of the uint32 block length, n is the payload length that
// follows it (branch tag + page bytes).
type pageLoc struct {
	off uint64
	n   uint32
}

// fileGen is one open inode of a .bsl file. A checkpoint publishes a new
// gen; snapshots that still point at the previous gen keep its fd alive
// so a rename does not invalidate in-flight reads.
type fileGen struct {
	f     *os.File
	wide  bool
	cache *pageCache
	loads atomic.Uint64

	errMu   sync.Mutex
	readErr error
}

func (g *fileGen) noteErr(err error) {
	if g == nil || err == nil {
		return
	}
	g.errMu.Lock()
	if g.readErr == nil {
		g.readErr = err
	}
	g.errMu.Unlock()
}

func (g *fileGen) err() error {
	if g == nil {
		return nil
	}
	g.errMu.Lock()
	defer g.errMu.Unlock()
	return g.readErr
}

func (g *fileGen) close() {
	if g == nil || g.f == nil {
		return
	}
	g.f.Close()
	g.f = nil
}

// diskLayer is the on-disk page directory for one branch layer. refs is
// immutable after publish. Loaded pages go into the shared cache, never
// into layerState.pages, so a published root stays immutable.
type diskLayer struct {
	gen    *fileGen
	branch string
	layer  int
	refs   map[uint64]pageLoc
}

func (d *diskLayer) load(idx uint64, loc pageLoc) (*Page, error) {
	if d == nil || d.gen == nil || d.gen.f == nil {
		return nil, fmt.Errorf("engine: database file is closed")
	}
	key := cacheKey{gen: d.gen, branch: d.branch, layer: d.layer, idx: idx}
	if d.gen.cache != nil {
		if p, ok := d.gen.cache.get(key); ok {
			return p, nil
		}
	}
	p, err := readPageAt(d.gen.f, loc, d.gen.wide)
	if err != nil {
		return nil, err
	}
	d.gen.loads.Add(1)
	if d.gen.cache != nil {
		d.gen.cache.put(key, p, int(loc.n))
	}
	return p, nil
}

func readPageAt(f *os.File, loc pageLoc, wide bool) (*Page, error) {
	buf := make([]byte, loc.n)
	if _, err := f.ReadAt(buf, int64(loc.off)+4); err != nil {
		return nil, fmt.Errorf("engine: read page at %d: %w", loc.off, err)
	}
	if len(buf) < 2 {
		return nil, fmt.Errorf("%w: short page block at %d", ErrCorruptPage, loc.off)
	}
	nameLen := int(binary.LittleEndian.Uint16(buf[:2]))
	if nameLen < 0 || 2+nameLen > len(buf) {
		return nil, fmt.Errorf("%w: bad branch tag at %d", ErrCorruptPage, loc.off)
	}
	return unmarshalPage(buf[2+nameLen:], wide)
}

type cacheKey struct {
	gen    *fileGen
	branch string
	layer  int
	idx    uint64
}

type cacheEntry struct {
	key  cacheKey
	page *Page
	cost int
}

// pageCache is a process-local LRU of decoded pages, keyed by file
// generation so a checkpoint cannot alias a page from the previous inode.
type pageCache struct {
	mu   sync.Mutex
	max  int
	used int
	ll   *list.List
	m    map[cacheKey]*list.Element
}

func newPageCache(max int) *pageCache {
	if max <= 0 {
		max = DefaultCacheBytes
	}
	return &pageCache{max: max, ll: list.New(), m: map[cacheKey]*list.Element{}}
}

func (c *pageCache) setMax(n int) {
	if c == nil {
		return
	}
	if n <= 0 {
		n = DefaultCacheBytes
	}
	c.mu.Lock()
	c.max = n
	c.evict()
	c.mu.Unlock()
}

func (c *pageCache) get(key cacheKey) (*Page, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).page, true
}

func (c *pageCache) put(key cacheKey, p *Page, cost int) {
	if c == nil || p == nil || cost <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		return
	}
	// A single page larger than the budget is not cached. Callers still
	// receive it; the next read decodes it again. That keeps the cap
	// honest for -max-memory.
	if cost > c.max {
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: key, page: p, cost: cost})
	c.m[key] = el
	c.used += cost
	c.evict()
}

func (c *pageCache) evict() {
	for c.used > c.max && c.ll.Len() > 0 {
		el := c.ll.Back()
		ent := el.Value.(*cacheEntry)
		c.ll.Remove(el)
		delete(c.m, ent.key)
		c.used -= ent.cost
	}
}

func (c *pageCache) dropGen(g *fileGen) {
	if c == nil || g == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, el := range c.m {
		if key.gen != g {
			continue
		}
		ent := el.Value.(*cacheEntry)
		c.ll.Remove(el)
		delete(c.m, key)
		c.used -= ent.cost
	}
}

// layerMeta is the persisted, page-free description of one layer: the
// directory, the id index, and flattened secondary indexes.
type layerMeta struct {
	height   int
	nextSlot uint64
	count    int
	refs     map[uint64]pageLoc
	idIndex  map[uint64]uint64
	indexes  map[string]*attrIndex
}

const indexMagic = "BSLI"
const indexVersion = uint32(1)

func encodeIndex(branches []string, metas map[string]map[int]*layerMeta) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 8)
	putU16 := func(n int) error {
		if n < 0 || n > 0xFFFF {
			return fmt.Errorf("engine: index field length %d does not fit uint16", n)
		}
		binary.LittleEndian.PutUint16(tmp[:2], uint16(n))
		buf = append(buf, tmp[:2]...)
		return nil
	}
	putU32 := func(n uint32) {
		binary.LittleEndian.PutUint32(tmp[:4], n)
		buf = append(buf, tmp[:4]...)
	}
	putU64 := func(n uint64) {
		binary.LittleEndian.PutUint64(tmp[:8], n)
		buf = append(buf, tmp[:8]...)
	}
	buf = append(buf, indexMagic...)
	putU32(indexVersion)
	putU32(uint32(len(branches)))
	for _, branch := range branches {
		if err := putU16(len(branch)); err != nil {
			return nil, err
		}
		buf = append(buf, branch...)
		layers := metas[branch]
		heights := make([]int, 0, len(layers))
		for h := range layers {
			heights = append(heights, h)
		}
		sort.Ints(heights)
		putU32(uint32(len(heights)))
		for _, h := range heights {
			lm := layers[h]
			putU32(uint32(lm.height))
			putU64(lm.nextSlot)
			putU64(uint64(lm.count))
			idxs := make([]uint64, 0, len(lm.refs))
			for idx := range lm.refs {
				idxs = append(idxs, idx)
			}
			sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
			putU32(uint32(len(idxs)))
			for _, idx := range idxs {
				loc := lm.refs[idx]
				putU64(idx)
				putU64(loc.off)
				putU32(loc.n)
			}
			ids := make([]uint64, 0, len(lm.idIndex))
			for id := range lm.idIndex {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			putU64(uint64(len(ids)))
			for _, id := range ids {
				putU64(id)
				putU64(lm.idIndex[id])
			}
			names := make([]string, 0, len(lm.indexes))
			for name := range lm.indexes {
				names = append(names, name)
			}
			sort.Strings(names)
			putU32(uint32(len(names)))
			for _, name := range names {
				ix := lm.indexes[name]
				if ix == nil {
					ix = newAttrIndex()
				}
				if err := putU16(len(name)); err != nil {
					return nil, err
				}
				buf = append(buf, name...)
				vals := ix.values()
				putU32(uint32(len(vals)))
				for _, v := range vals {
					if len(v) > int(^uint32(0)) {
						return nil, fmt.Errorf("engine: index value for %q is too long", name)
					}
					putU32(uint32(len(v)))
					buf = append(buf, v...)
					ids := ix.lookup(v)
					putU32(uint32(len(ids)))
					for _, id := range ids {
						putU64(id)
					}
				}
			}
		}
	}
	return buf, nil
}

func decodeIndex(data []byte, gen *fileGen) (map[string]map[int]*layerState, uint64, error) {
	if len(data) < 8 || string(data[:4]) != indexMagic {
		return nil, 0, fmt.Errorf("%w: bad index magic", ErrCorruptHeader)
	}
	r := &indexReader{b: data, i: 4}
	if v := r.u32(); v != indexVersion {
		return nil, 0, fmt.Errorf("%w: index version %d", ErrCorruptHeader, v)
	}
	nBranch := int(r.u32())
	states := map[string]map[int]*layerState{}
	var maxID uint64
	for b := 0; b < nBranch && r.err == nil; b++ {
		branch := r.str16()
		nLayer := int(r.u32())
		lsMap := map[int]*layerState{}
		states[branch] = lsMap
		for li := 0; li < nLayer && r.err == nil; li++ {
			height := int(r.u32())
			nextSlot := r.u64()
			count := int(r.u64())
			nPage := int(r.u32())
			refs := make(map[uint64]pageLoc, nPage)
			for p := 0; p < nPage && r.err == nil; p++ {
				idx := r.u64()
				off := r.u64()
				n := r.u32()
				refs[idx] = pageLoc{off: off, n: n}
			}
			nID := r.u64()
			idIndex := make(map[uint64]uint64, nID)
			for i := uint64(0); i < nID && r.err == nil; i++ {
				id := r.u64()
				slot := r.u64()
				idIndex[id] = slot
				if id > maxID {
					maxID = id
				}
			}
			nIdx := int(r.u32())
			indexes := map[string]*attrIndex{}
			for i := 0; i < nIdx && r.err == nil; i++ {
				name := r.str16()
				nVal := int(r.u32())
				ix := newAttrIndex()
				for v := 0; v < nVal && r.err == nil; v++ {
					val := r.str32()
					nIDs := int(r.u32())
					for k := 0; k < nIDs && r.err == nil; k++ {
						id := r.u64()
						ix.add(val, id)
						if id > maxID {
							maxID = id
						}
					}
				}
				indexes[name] = ix
			}
			ls := newLayerState()
			ls.nextSlot = nextSlot
			ls.count = count
			ls.idIndex = idIndex
			ls.indexes = indexes
			if len(refs) > 0 {
				ls.disk = &diskLayer{gen: gen, branch: branch, layer: height, refs: refs}
			}
			lsMap[height] = ls
		}
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	if r.i != len(r.b) {
		return nil, 0, fmt.Errorf("%w: trailing index bytes", ErrCorruptHeader)
	}
	return states, maxID, nil
}

type indexReader struct {
	b   []byte
	i   int
	err error
}

func (r *indexReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.i+n > len(r.b) {
		r.err = fmt.Errorf("%w: truncated index", ErrCorruptHeader)
		return false
	}
	return true
}

func (r *indexReader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.i:])
	r.i += 2
	return v
}

func (r *indexReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.i:])
	r.i += 4
	return v
}

func (r *indexReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.i:])
	r.i += 8
	return v
}

func (r *indexReader) bytes(n int) string {
	if !r.need(n) {
		return ""
	}
	s := string(r.b[r.i : r.i+n])
	r.i += n
	return s
}

func (r *indexReader) str16() string { return r.bytes(int(r.u16())) }
func (r *indexReader) str32() string { return r.bytes(int(r.u32())) }

// ParseByteSize parses a memory budget: a raw byte count, or a number with
// a suffix K, KB, KiB, M, MB, MiB, G, GB, or GiB (1024-based).
func ParseByteSize(s string) (int64, error) {
	s = trimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("engine: empty byte size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("engine: bad byte size %q", s)
	}
	n, err := parseInt64(s[:i])
	if err != nil {
		return 0, fmt.Errorf("engine: bad byte size %q", s)
	}
	suf := s[i:]
	var mul int64 = 1
	switch suf {
	case "", "B", "b":
		mul = 1
	case "K", "k", "KB", "kb", "KiB", "kib":
		mul = 1 << 10
	case "M", "m", "MB", "mb", "MiB", "mib":
		mul = 1 << 20
	case "G", "g", "GB", "gb", "GiB", "gib":
		mul = 1 << 30
	default:
		return 0, fmt.Errorf("engine: bad byte size %q", s)
	}
	if n > (1<<62)/mul {
		return 0, fmt.Errorf("engine: byte size %q overflows", s)
	}
	return n * mul, nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// scanDataPages decodes each page once, keeps the directory and indexes,
// and drops the page. Used when a file has no index region yet.
func scanDataPages(f *os.File, hdr *Header, gen *fileGen) (map[string]map[int]*layerState, uint64, error) {
	states := map[string]map[int]*layerState{}
	var maxID uint64
	off := hdr.DataStart
	var lenb [4]byte
	for off < hdr.DataEnd {
		if _, err := f.ReadAt(lenb[:], int64(off)); err != nil {
			return nil, 0, fmt.Errorf("%w: truncated page block header: %v", ErrCorruptPage, err)
		}
		blockLen := binary.LittleEndian.Uint32(lenb[:])
		if blockLen < 2 || off+4+uint64(blockLen) > hdr.DataEnd {
			return nil, 0, fmt.Errorf("%w: truncated page block (%d bytes)", ErrCorruptPage, blockLen)
		}
		loc := pageLoc{off: off, n: blockLen}
		p, err := readPageAt(f, loc, gen.wide)
		if err != nil {
			return nil, 0, err
		}
		branch, err := readBlockBranch(f, loc)
		if err != nil {
			return nil, 0, err
		}
		lsMap := states[branch]
		if lsMap == nil {
			lsMap = map[int]*layerState{}
			states[branch] = lsMap
		}
		ls := lsMap[p.Layer]
		if ls == nil {
			ls = newLayerState()
			if def, ok := hdr.Layers[p.Layer]; ok {
				ls.bindSchema(def)
			}
			ls.disk = &diskLayer{gen: gen, branch: branch, layer: p.Layer, refs: map[uint64]pageLoc{}}
			lsMap[p.Layer] = ls
		}
		idx := p.SlotBase / SlotsPerPage
		ls.disk.refs[idx] = loc
		def := hdr.Layers[p.Layer]
		for _, rec := range p.Slots() {
			ls.absorb(def, rec)
			if rec.ID > maxID {
				maxID = rec.ID
			}
		}
		off += 4 + uint64(blockLen)
	}
	return states, maxID, nil
}

func readBlockBranch(f *os.File, loc pageLoc) (string, error) {
	if loc.n < 2 {
		return "", fmt.Errorf("%w: short page block", ErrCorruptPage)
	}
	hdr := make([]byte, 2)
	if _, err := f.ReadAt(hdr, int64(loc.off)+4); err != nil && err != io.EOF {
		return "", err
	}
	n := int(binary.LittleEndian.Uint16(hdr))
	if n <= 0 || 2+n > int(loc.n) {
		return "", fmt.Errorf("%w: bad branch tag", ErrCorruptPage)
	}
	name := make([]byte, n)
	if _, err := f.ReadAt(name, int64(loc.off)+6); err != nil && err != io.EOF {
		return "", err
	}
	return string(name), nil
}
