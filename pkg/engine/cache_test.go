package engine

import (
	"sync"
	"testing"
)

func TestPageCacheShardCount(t *testing.T) {
	for _, tc := range []struct{ max, want int }{
		{1, 1},
		{minShardBudget, 1},
		{4 * minShardBudget, 4},
		{DefaultCacheBytes, maxCacheShards},
		{1 << 40, maxCacheShards},
	} {
		if got := shardCount(tc.max); got != tc.want {
			t.Errorf("shardCount(%d) = %d, want %d", tc.max, got, tc.want)
		}
	}
}

func TestPageCacheBudgetAndDropGen(t *testing.T) {
	c := newPageCache(DefaultCacheBytes)
	g1, g2 := &fileGen{}, &fileGen{}
	for i := uint64(0); i < 200; i++ {
		c.put(cacheKey{gen: g1, layer: 1, idx: i}, &Page{}, 1000)
		c.put(cacheKey{gen: g2, layer: 1, idx: i}, &Page{}, 1000)
	}
	for i := uint64(0); i < 200; i++ {
		if _, ok := c.get(cacheKey{gen: g1, layer: 1, idx: i}); !ok {
			t.Fatalf("page %d missing", i)
		}
	}

	c.dropGen(g1)
	for i := uint64(0); i < 200; i++ {
		if _, ok := c.get(cacheKey{gen: g1, layer: 1, idx: i}); ok {
			t.Fatalf("page %d of dropped gen still cached", i)
		}
		if _, ok := c.get(cacheKey{gen: g2, layer: 1, idx: i}); !ok {
			t.Fatalf("page %d of live gen dropped", i)
		}
	}

	// Shrinking the budget evicts down to it.
	c.setMax(1)
	used := 0
	for _, sh := range *c.shards.Load() {
		used += sh.used
	}
	if used > 1 {
		t.Fatalf("used = %d after setMax(1)", used)
	}
}

func TestPageCacheConcurrent(t *testing.T) {
	c := newPageCache(DefaultCacheBytes)
	g := &fileGen{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := uint64(0); i < 2000; i++ {
				k := cacheKey{gen: g, layer: w % 3, idx: i % 97}
				if _, ok := c.get(k); !ok {
					c.put(k, &Page{}, 512)
				}
				if i%500 == 0 {
					c.setMax(DefaultCacheBytes / 2)
				}
			}
		}(w)
	}
	wg.Wait()
}
