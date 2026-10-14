package engine

import (
	"fmt"
)

// CascadeDelete invalidates the subtree rooted at (layerName, id) using the
// matrix cascade engine: the target record's child pointer array is read,
// then validity bits are flipped top-down across the slotted pages of each
// layer height with word-parallel bitmask range writes, and the affected
// id-index entries are dropped by id. Cost is O(subtree): the walk visits
// every descendant once and every index entry is removed directly, with no
// per-entry scan of the layer.
//
// The deletion is recorded in the commit block (Author, Summary,
// Reason) and replayed deterministically from the journal after a crash.
func (tx *WriteTx) Delete(layerName string, id uint64) (int, error) {
	if err := tx.check(); err != nil {
		return 0, err
	}
	height, err := tx.view.height(layerName)
	if err != nil {
		return 0, err
	}
	n, err := cascadeDelete(tx.view, height, id)
	if err != nil {
		return 0, err
	}
	tx.ops = append(tx.ops, Op{Kind: opDelete, Layer: height, ID: id})
	return n, nil
}

// cascadeDelete performs the top-down bitmask invalidation over an edit
// view. It is shared by write transactions and journal replay so both
// produce identical matrix states.
func cascadeDelete(e *editView, layer int, id uint64) (int, error) {
	type ref struct {
		layer int
		id    uint64
	}
	root, ok := e.Record(layer, id)
	if !ok {
		return 0, fmt.Errorf("engine: record %d not found in layer %q", id, e.hdr.layerName(layer))
	}

	// Top-down breadth-first walk of the child pointer arrays.
	visited := map[ref]bool{}
	level := []ref{{layer, id}}
	visited[level[0]] = true
	// Slots and ids to invalidate, grouped per layer for contiguous range
	// flips and direct id-index drops.
	perLayer := map[int][]uint64{}
	perLayerIDs := map[int][]uint64{}
	deleted := 0

	for len(level) > 0 {
		var next []ref
		for _, cur := range level {
			rec, ok := e.Record(cur.layer, cur.id)
			if !ok || rec == nil {
				continue
			}
			perLayer[cur.layer] = append(perLayer[cur.layer], rec.Slot)
			perLayerIDs[cur.layer] = append(perLayerIDs[cur.layer], rec.ID)
			deleted++
			if cur.layer+1 > len(e.hdr.Layers)-1 && len(rec.Children) > 0 {
				// Children must live at height+1; a missing layer means
				// the container is structurally corrupt.
				if _, ok := e.hdr.Layers[cur.layer+1]; !ok {
					return 0, fmt.Errorf("engine: record %d has children but no layer below %q",
						rec.ID, e.hdr.layerName(cur.layer))
				}
			}
			for _, child := range rec.Children {
				r := ref{cur.layer + 1, child}
				if !visited[r] {
					visited[r] = true
					next = append(next, r)
				}
			}
		}
		level = next
	}

	// Detach the subtree root from its parent's child pointer array.
	if root.Parent != 0 && layer > 0 {
		if p := e.recordForWrite(layer-1, root.Parent); p != nil {
			out := p.Children[:0]
			for _, c := range p.Children {
				if c != root.ID {
					out = append(out, c)
				}
			}
			p.Children = out
		}
	}

	// Top-down bitmask pruning: flip validity words over contiguous slot
	// runs per layer, then drop the ID index entries.
	for _, height := range e.hdr.LayerHeights() {
		slots := sortedSlots(perLayer[height])
		for i := 0; i < len(slots); {
			j := i
			for j+1 < len(slots) && slots[j+1] == slots[j]+1 {
				j++
			}
			e.invalidateRange(height, slots[i], slots[j]-slots[i]+1)
			i = j + 1
		}
		ls := e.layers[height]
		if ls == nil {
			continue
		}
		// The BFS already collected every deleted id at this height, so
		// index cleanup is a direct map delete per record: O(subtree).
		for _, id := range perLayerIDs[height] {
			delete(ls.idIndex, id)
		}
	}
	return deleted, nil
}
