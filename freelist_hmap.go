package bbolt

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/MPCherry/bbolt/internal/common"
)

// hashmapFreeCount returns count of free pages(hashmap version)
func (f *freelist) hashmapFreeCount() int {
	common.Verify(func() {
		expectedFreePageCount := f.hashmapFreeCountSlow()
		common.Assert(int(f.freePagesCount) == expectedFreePageCount,
			"freePagesCount (%d) is out of sync with free pages map (%d)", f.freePagesCount, expectedFreePageCount)
	})
	return int(f.freePagesCount)
}

func (f *freelist) hashmapFreeCountSlow() int {
	count := 0
	for _, size := range f.forwardMap {
		count += int(size)
	}
	return count
}

// hashmapAllocate serves the same purpose as arrayAllocate, but use hashmap as backend
func (f *freelist) hashmapAllocate(txid common.Txid, n int) common.Pgid {
	if n == 0 {
		return 0
	}

	// if we have a exact size match just return short path
	if bm, ok := f.freemaps[uint64(n)]; ok {
		for pid := range bm {
			// remove the span
			f.delSpan(pid, uint64(n))

			f.allocs[pid] = txid

			for i := common.Pgid(0); i < common.Pgid(n); i++ {
				delete(f.cache, pid+i)
			}
			return pid
		}
	}

	// lookup the map to find larger span
	for size, bm := range f.freemaps {
		if size < uint64(n) {
			continue
		}

		for pid := range bm {
			// remove the initial
			f.delSpan(pid, size)

			f.allocs[pid] = txid

			remain := size - uint64(n)

			// add remain span
			f.addSpan(pid+common.Pgid(n), remain)

			for i := common.Pgid(0); i < common.Pgid(n); i++ {
				delete(f.cache, pid+i)
			}
			return pid
		}
	}

	return 0
}

// hashmapReadIDs reads pgids as input an initial the freelist(hashmap version)
func (f *freelist) hashmapReadIDs(pgids []common.Pgid) {
	f.init(pgids)

	// Rebuild the page cache.
	f.reindex()
}

// hashmapGetFreePageIDs returns the sorted free page ids
func (f *freelist) hashmapGetFreePageIDs() []common.Pgid {
	count := f.free_count()
	if count == 0 {
		return nil
	}

	m := make([]common.Pgid, 0, count)

	startPageIds := make([]common.Pgid, 0, len(f.forwardMap))
	for k := range f.forwardMap {
		startPageIds = append(startPageIds, k)
	}
	sort.Sort(common.Pgids(startPageIds))

	for _, start := range startPageIds {
		if size, ok := f.forwardMap[start]; ok {
			for i := 0; i < int(size); i++ {
				m = append(m, start+common.Pgid(i))
			}
		}
	}

	return m
}

// hashmapMergeSpans try to merge list of pages(represented by pgids) with existing spans
func (f *freelist) hashmapMergeSpans(ids common.Pgids) {
	common.Verify(func() {
		ids1Freemap := f.idsFromFreemaps()
		ids2Forward := f.idsFromForwardMap()
		ids3Backward := f.idsFromBackwardMap()

		if !reflect.DeepEqual(ids1Freemap, ids2Forward) {
			panic(fmt.Sprintf("Detected mismatch, f.freemaps: %v, f.forwardMap: %v", f.freemaps, f.forwardMap))
		}
		if !reflect.DeepEqual(ids1Freemap, ids3Backward) {
			panic(fmt.Sprintf("Detected mismatch, f.freemaps: %v, f.backwardMap: %v", f.freemaps, f.backwardMap))
		}

		sort.Sort(ids)
		prev := common.Pgid(0)
		for _, id := range ids {
			// The ids shouldn't have duplicated free ID.
			if prev == id {
				panic(fmt.Sprintf("detected duplicated free ID: %d in ids: %v", id, ids))
			}
			prev = id

			// The ids shouldn't have any overlap with the existing f.freemaps.
			if _, ok := ids1Freemap[id]; ok {
				panic(fmt.Sprintf("detected overlapped free page ID: %d between ids: %v and existing f.freemaps: %v", id, ids, f.freemaps))
			}
		}
	})
	for _, id := range ids {
		// try to see if we can merge and update
		f.mergeWithExistingSpan(id)
	}
}

// mergeWithExistingSpan merges pid to the existing free spans, try to merge it backward and forward
func (f *freelist) mergeWithExistingSpan(pid common.Pgid) {
	prev := pid - 1
	next := pid + 1

	preSize, mergeWithPrev := f.backwardMap[prev]
	nextSize, mergeWithNext := f.forwardMap[next]
	newStart := pid
	newSize := uint64(1)

	if mergeWithPrev {
		//merge with previous span
		start := prev + 1 - common.Pgid(preSize)
		f.delSpan(start, preSize)

		newStart -= common.Pgid(preSize)
		newSize += preSize
	}

	if mergeWithNext {
		// merge with next span
		f.delSpan(next, nextSize)
		newSize += nextSize
	}

	f.addSpan(newStart, newSize)
}

func (f *freelist) addSpan(start common.Pgid, size uint64) {
	f.backwardMap[start-1+common.Pgid(size)] = size
	f.forwardMap[start] = size
	if _, ok := f.freemaps[size]; !ok {
		f.freemaps[size] = make(map[common.Pgid]struct{})
	}

	f.freemaps[size][start] = struct{}{}
	f.freePagesCount += size
}

func (f *freelist) delSpan(start common.Pgid, size uint64) {
	delete(f.forwardMap, start)
	delete(f.backwardMap, start+common.Pgid(size-1))
	delete(f.freemaps[size], start)
	if len(f.freemaps[size]) == 0 {
		delete(f.freemaps, size)
	}
	f.freePagesCount -= size
}

// initial from pgids using when use hashmap version
// pgids must be sorted
func (f *freelist) init(pgids []common.Pgid) {
	if len(pgids) == 0 {
		return
	}

	size := uint64(1)
	start := pgids[0]
	// reset the counter when freelist init
	f.freePagesCount = 0

	if !sort.SliceIsSorted([]common.Pgid(pgids), func(i, j int) bool { return pgids[i] < pgids[j] }) {
		panic("pgids not sorted")
	}

	f.freemaps = make(map[uint64]pidSet)
	f.forwardMap = make(map[common.Pgid]uint64)
	f.backwardMap = make(map[common.Pgid]uint64)

	for i := 1; i < len(pgids); i++ {
		// continuous page
		if pgids[i] == pgids[i-1]+1 {
			size++
		} else {
			f.addSpan(start, size)

			size = 1
			start = pgids[i]
		}
	}

	// init the tail
	if size != 0 && start != 0 {
		f.addSpan(start, size)
	}
}

// idsFromFreemaps get all free page IDs from f.freemaps.
// used by test only.
func (f *freelist) idsFromFreemaps() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for size, idSet := range f.freemaps {
		for start := range idSet {
			for i := 0; i < int(size); i++ {
				id := start + common.Pgid(i)
				if _, ok := ids[id]; ok {
					panic(fmt.Sprintf("detected duplicated free page ID: %d in f.freemaps: %v", id, f.freemaps))
				}
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

// idsFromForwardMap get all free page IDs from f.forwardMap.
// used by test only.
func (f *freelist) idsFromForwardMap() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for start, size := range f.forwardMap {
		for i := 0; i < int(size); i++ {
			id := start + common.Pgid(i)
			if _, ok := ids[id]; ok {
				panic(fmt.Sprintf("detected duplicated free page ID: %d in f.forwardMap: %v", id, f.forwardMap))
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}

// idsFromBackwardMap get all free page IDs from f.backwardMap.
// used by test only.
func (f *freelist) idsFromBackwardMap() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for end, size := range f.backwardMap {
		for i := 0; i < int(size); i++ {
			id := end - common.Pgid(i)
			if _, ok := ids[id]; ok {
				panic(fmt.Sprintf("detected duplicated free page ID: %d in f.backwardMap: %v", id, f.backwardMap))
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}
