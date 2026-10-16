package engine

import "sort"

// attrIndex is a hash index from one attribute's stored string value to
// the record ids that hold it. It is a copy-on-write chain, same idea as
// layerState: a published node is never mutated. An overlay records ids it
// added (put) and ids it hid from older nodes (del).
//
// del hides older levels only. An id put at this level stays visible even
// when del also contains it, which is how an update that removes the old
// value and inserts a new one stays correct without resurrecting the old
// value still stored on a parent node.
type attrIndex struct {
	base *attrIndex
	put  map[string]map[uint64]struct{}
	del  map[uint64]struct{}
}

func newAttrIndex() *attrIndex {
	return &attrIndex{put: map[string]map[uint64]struct{}{}}
}

// add records id under val at this node. The caller must pass a node it
// owns (see layerState.indexForWrite); this never writes to base.
func (a *attrIndex) add(val string, id uint64) {
	if a.put == nil {
		a.put = map[string]map[uint64]struct{}{}
	}
	m := a.put[val]
	if m == nil {
		m = map[uint64]struct{}{}
		a.put[val] = m
	}
	m[id] = struct{}{}
}

// drop hides id from older nodes and forgets it in this node's put for
// val. It does not scan other values: the caller knows the value the
// record currently stores.
func (a *attrIndex) drop(val string, id uint64) {
	if m := a.put[val]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(a.put, val)
		}
	}
	if a.base == nil {
		return
	}
	if a.del == nil {
		a.del = map[uint64]struct{}{}
	}
	a.del[id] = struct{}{}
}

// lookup returns the live ids for val in ascending id order. Order matches
// slot order within a layer, which is what limit and sort=desc rely on.
func (a *attrIndex) lookup(val string) []uint64 {
	if a == nil {
		return nil
	}
	hidden := map[uint64]struct{}{}
	seen := map[uint64]struct{}{}
	var ids []uint64
	for cur := a; cur != nil; cur = cur.base {
		for id := range cur.put[val] {
			if _, skip := hidden[id]; skip {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		for id := range cur.del {
			hidden[id] = struct{}{}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// values returns every distinct key that appears anywhere in the chain.
func (a *attrIndex) values() []string {
	seen := map[string]struct{}{}
	var out []string
	for cur := a; cur != nil; cur = cur.base {
		for v := range cur.put {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// flatten copies the live index into a single root node so a published
// snapshot does not share a writable put map with its parent.
func (a *attrIndex) flatten() *attrIndex {
	if a == nil {
		return newAttrIndex()
	}
	out := newAttrIndex()
	for _, v := range a.values() {
		for _, id := range a.lookup(v) {
			out.add(v, id)
		}
	}
	return out
}
