package engine

import (
	"bufio"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
)

// DefaultCacheBytes is the decoded-page cache budget. Secondary attribute
// indexes stay resident. The id index is a mapped sorted array, not part
// of this budget. The cap bounds slotted pages only.
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

	mapped []byte // whole file, read-only; nil when the mapping failed or was dropped
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
	if g == nil {
		return
	}
	g.unmap()
	if g.f == nil {
		return
	}
	g.f.Close()
	g.f = nil
}

// mapFile maps the open file so the id index can be binary-searched without
// copying it into a Go map. Mapping does not read the pages.
func (g *fileGen) mapFile(size int64) error {
	if g == nil || g.f == nil || size <= 0 {
		return nil
	}
	b, err := syscall.Mmap(int(g.f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("engine: map database: %w", err)
	}
	g.mapped = b
	return nil
}

func (g *fileGen) unmap() {
	if g == nil || g.mapped == nil {
		return
	}
	_ = syscall.Munmap(g.mapped)
	g.mapped = nil
}

// idDisk is a sorted id→slot array inside a mapped index region.
// Each entry is 16 bytes: id uint64, slot uint64, little-endian.
type idDisk struct {
	gen *fileGen // keeps the mapping alive for as long as lookups use b
	b   []byte
}

func (d *idDisk) length() int {
	if d == nil {
		return 0
	}
	return len(d.b) / 16
}

func (d *idDisk) at(i int) (id, slot uint64) {
	off := i * 16
	return binary.LittleEndian.Uint64(d.b[off:]), binary.LittleEndian.Uint64(d.b[off+8:])
}

func (d *idDisk) find(id uint64) (uint64, bool) {
	if d == nil || len(d.b) < 16 {
		return 0, false
	}
	n := len(d.b) / 16
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		off := mid * 16
		got := binary.LittleEndian.Uint64(d.b[off:])
		if got < id {
			lo = mid + 1
			continue
		}
		if got > id {
			hi = mid
			continue
		}
		return binary.LittleEndian.Uint64(d.b[off+8:]), true
	}
	return 0, false
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
// It is split into shards with their own locks so parallel readers do not
// queue on one mutex. Each shard holds an equal slice of the budget, so
// eviction is LRU per shard rather than global.
type pageCache struct {
	shards atomic.Pointer[[]*cacheShard]
	mu     sync.Mutex // serializes setMax and dropGen
	max    int
}

type cacheShard struct {
	mu   sync.Mutex
	max  int
	used int
	ll   *list.List
	m    map[cacheKey]*list.Element
}

const (
	maxCacheShards = 16
	minShardBudget = 2 << 20 // keep room for several pages per shard
)

func newPageCache(max int) *pageCache {
	c := &pageCache{}
	c.mu.Lock()
	c.resize(max)
	c.mu.Unlock()
	return c
}

// shardCount picks fewer shards for small budgets so a single shard can
// still hold whole pages.
func shardCount(max int) int {
	n := max / minShardBudget
	if n < 1 {
		return 1
	}
	if n > maxCacheShards {
		return maxCacheShards
	}
	return n
}

// resize applies a new budget. Callers hold c.mu. Changing the shard
// count starts an empty cache; pages are reloaded on demand.
func (c *pageCache) resize(max int) {
	if max <= 0 {
		max = DefaultCacheBytes
	}
	c.max = max
	n := shardCount(max)
	per := max / n
	if cur := c.shards.Load(); cur != nil && len(*cur) == n {
		for _, sh := range *cur {
			sh.mu.Lock()
			sh.max = per
			sh.evict()
			sh.mu.Unlock()
		}
		return
	}
	shards := make([]*cacheShard, n)
	for i := range shards {
		shards[i] = &cacheShard{max: per, ll: list.New(), m: map[cacheKey]*list.Element{}}
	}
	c.shards.Store(&shards)
}

func (c *pageCache) setMax(n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.resize(n)
	c.mu.Unlock()
}

func (c *pageCache) shard(key cacheKey) *cacheShard {
	shards := *c.shards.Load()
	if len(shards) == 1 {
		return shards[0]
	}
	h := (key.idx*0x9E3779B97F4A7C15 ^ uint64(key.layer)*0xC2B2AE3D27D4EB4F) >> 32
	return shards[h%uint64(len(shards))]
}

func (c *pageCache) get(key cacheKey) (*Page, bool) {
	sh := c.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	el, ok := sh.m[key]
	if !ok {
		return nil, false
	}
	sh.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).page, true
}

func (c *pageCache) put(key cacheKey, p *Page, cost int) {
	if c == nil || p == nil || cost <= 0 {
		return
	}
	sh := c.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.m[key]; ok {
		sh.ll.MoveToFront(el)
		return
	}
	// A page larger than the shard budget is not cached. Callers still
	// receive it; the next read decodes it again. That keeps the cap
	// honest for -max-memory.
	if cost > sh.max {
		return
	}
	el := sh.ll.PushFront(&cacheEntry{key: key, page: p, cost: cost})
	sh.m[key] = el
	sh.used += cost
	sh.evict()
}

func (sh *cacheShard) evict() {
	for sh.used > sh.max && sh.ll.Len() > 0 {
		el := sh.ll.Back()
		ent := el.Value.(*cacheEntry)
		sh.ll.Remove(el)
		delete(sh.m, ent.key)
		sh.used -= ent.cost
	}
}

func (c *pageCache) dropGen(g *fileGen) {
	if c == nil || g == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sh := range *c.shards.Load() {
		sh.mu.Lock()
		for key, el := range sh.m {
			if key.gen != g {
				continue
			}
			ent := el.Value.(*cacheEntry)
			sh.ll.Remove(el)
			delete(sh.m, key)
			sh.used -= ent.cost
		}
		sh.mu.Unlock()
	}
}

// layerMeta is the persisted, page-free description of one layer: the
// directory, the id index, and flattened secondary indexes.
type layerMeta struct {
	height   int
	nextSlot uint64
	count    int
	refs     map[uint64]pageLoc
	src      *layerState // live layer; ids are streamed from it
	idCount  uint64      // set with idRead to stream pairs without a layer
	idRead   io.Reader
	indexes  map[string]*attrIndex
}

const indexMagic = "BSLI"
const indexVersion = uint32(1)

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeIndex writes the BSLI index region. Id pairs are streamed from the
// live layer (or from idRead) in id order; they are not assembled as one map.
func writeIndex(w io.Writer, branches []string, metas map[string]map[int]*layerMeta) (int64, error) {
	bw := bufio.NewWriterSize(w, 1<<20)
	cw := &countingWriter{w: bw}
	tmp := make([]byte, 8)
	putU16 := func(n int) error {
		if n < 0 || n > 0xFFFF {
			return fmt.Errorf("engine: index field length %d does not fit uint16", n)
		}
		binary.LittleEndian.PutUint16(tmp[:2], uint16(n))
		_, err := cw.Write(tmp[:2])
		return err
	}
	putU32 := func(n uint32) error {
		binary.LittleEndian.PutUint32(tmp[:4], n)
		_, err := cw.Write(tmp[:4])
		return err
	}
	putU64 := func(n uint64) error {
		binary.LittleEndian.PutUint64(tmp[:8], n)
		_, err := cw.Write(tmp[:8])
		return err
	}
	if _, err := cw.Write([]byte(indexMagic)); err != nil {
		return 0, err
	}
	if err := putU32(indexVersion); err != nil {
		return 0, err
	}
	if err := putU32(uint32(len(branches))); err != nil {
		return 0, err
	}
	for _, branch := range branches {
		if err := putU16(len(branch)); err != nil {
			return 0, err
		}
		if _, err := cw.Write([]byte(branch)); err != nil {
			return 0, err
		}
		layers := metas[branch]
		heights := make([]int, 0, len(layers))
		for h := range layers {
			heights = append(heights, h)
		}
		sort.Ints(heights)
		if err := putU32(uint32(len(heights))); err != nil {
			return 0, err
		}
		for _, h := range heights {
			lm := layers[h]
			if err := putU32(uint32(lm.height)); err != nil {
				return 0, err
			}
			if err := putU64(lm.nextSlot); err != nil || putU64(uint64(lm.count)) != nil {
				return 0, fmt.Errorf("engine: write index counts")
			}
			idxs := make([]uint64, 0, len(lm.refs))
			for idx := range lm.refs {
				idxs = append(idxs, idx)
			}
			sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
			if err := putU32(uint32(len(idxs))); err != nil {
				return 0, err
			}
			for _, idx := range idxs {
				loc := lm.refs[idx]
				if err := putU64(idx); err != nil {
					return 0, err
				}
				if err := putU64(loc.off); err != nil {
					return 0, err
				}
				if err := putU32(loc.n); err != nil {
					return 0, err
				}
			}
			if err := writeIDSection(cw, putU64, lm); err != nil {
				return 0, err
			}
			names := make([]string, 0, len(lm.indexes))
			for name := range lm.indexes {
				names = append(names, name)
			}
			sort.Strings(names)
			if err := putU32(uint32(len(names))); err != nil {
				return 0, err
			}
			for _, name := range names {
				ix := lm.indexes[name]
				if ix == nil {
					ix = newAttrIndex()
				}
				if err := putU16(len(name)); err != nil {
					return 0, err
				}
				if _, err := cw.Write([]byte(name)); err != nil {
					return 0, err
				}
				vals := ix.values()
				if err := putU32(uint32(len(vals))); err != nil {
					return 0, err
				}
				for _, v := range vals {
					if len(v) > int(^uint32(0)) {
						return 0, fmt.Errorf("engine: index value for %q is too long", name)
					}
					if err := putU32(uint32(len(v))); err != nil {
						return 0, err
					}
					if _, err := cw.Write([]byte(v)); err != nil {
						return 0, err
					}
					ids := ix.lookup(v)
					if err := putU32(uint32(len(ids))); err != nil {
						return 0, err
					}
					for _, id := range ids {
						if err := putU64(id); err != nil {
							return 0, err
						}
					}
				}
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return cw.n, nil
}

func writeIDSection(w io.Writer, putU64 func(uint64) error, lm *layerMeta) error {
	if lm.idRead != nil {
		if err := putU64(lm.idCount); err != nil {
			return err
		}
		_, err := io.CopyN(w, lm.idRead, int64(lm.idCount)*16)
		return err
	}
	pairs := mergeIDs(lm.src)
	if err := putU64(uint64(len(pairs))); err != nil {
		return err
	}
	for _, p := range pairs {
		if err := putU64(p.id); err != nil {
			return err
		}
		if err := putU64(p.slot); err != nil {
			return err
		}
	}
	return nil
}

type idSlot struct {
	id   uint64
	slot uint64
}

// mergeIDs returns live id→slot pairs in id order. Disk pairs are read in
// order and overridden by in-memory inserts, updates, and deletions.
func mergeIDs(l *layerState) []idSlot {
	if l == nil {
		return nil
	}
	dels := map[uint64]bool{}
	seen := map[uint64]struct{}{}
	var mem []idSlot
	var disk *idDisk
	var chain []*layerState
	for cur := l; cur != nil; cur = cur.base {
		chain = append(chain, cur)
		for id := range cur.dels {
			dels[id] = true
		}
		if cur.idDisk != nil {
			disk = cur.idDisk
		}
	}
	for _, cur := range chain {
		for id, slot := range cur.idIndex {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if dels[id] {
				continue
			}
			mem = append(mem, idSlot{id: id, slot: slot})
		}
	}
	sort.Slice(mem, func(i, j int) bool { return mem[i].id < mem[j].id })
	if disk == nil || disk.length() == 0 {
		return mem
	}
	out := make([]idSlot, 0, disk.length())
	di, pi := 0, 0
	dn := disk.length()
	for di < dn || pi < len(mem) {
		if pi < len(mem) {
			takeMem := di >= dn
			if !takeMem {
				did, _ := disk.at(di)
				takeMem = mem[pi].id <= did
			}
			if takeMem {
				id, slot := mem[pi].id, mem[pi].slot
				if di < dn {
					did, _ := disk.at(di)
					if did == id {
						di++
					}
				}
				pi++
				if !dels[id] {
					out = append(out, idSlot{id: id, slot: slot})
				}
				continue
			}
		}
		id, slot := disk.at(di)
		di++
		if dels[id] {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, idSlot{id: id, slot: slot})
	}
	return out
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
			var disk *idDisk
			if nID > 0 && r.err == nil {
				if nID > uint64(len(r.b))/16 || r.i > len(r.b) || uint64(len(r.b)-r.i) < nID*16 {
					r.err = fmt.Errorf("%w: truncated id index", ErrCorruptHeader)
				} else {
					start := r.i
					r.i += int(nID) * 16
					disk = &idDisk{gen: gen, b: r.b[start:r.i]}
					last, _ := disk.at(disk.length() - 1)
					if last > maxID {
						maxID = last
					}
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
			ls.idDisk = disk
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
