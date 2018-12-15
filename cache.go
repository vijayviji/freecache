package freecache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash"
)

const (
	// segmentCount represents the number of segments within a freecache instance.
	segmentCount = 256
	// segmentAndOpVal is bitwise AND applied to the hashVal to find the segment id.
	segmentAndOpVal = 255
	minBufSize      = 512 * 1024
)

// Cache is a freecache instance.
type Cache struct {
	locks    [segmentCount]sync.Mutex
	segments [segmentCount]segment
}

func hashFunc(data []byte) uint64 {
	return xxhash.Sum64(data)
}

// NewCache returns a newly initialize cache by size.
// The cache size will be set to 512KB at minimum.
// If the size is set relatively large, you should call
// `debug.SetGCPercent()`, set it to a much smaller value
// to limit the memory consumption and GC pause time.
func NewCache(size int) (cache *Cache) {
	if size < minBufSize {
		size = minBufSize
	}
	cache = new(Cache)
	for i := 0; i < segmentCount; i++ {
		cache.segments[i] = newSegment(size/segmentCount, i)
	}
	return
}

// Set sets a key, value and expiration for a cache entry and stores it in the cache.
// If the key is larger than 65535 or value is larger than 1/1024 of the cache size,
// the entry will not be written to the cache. expireSeconds <= 0 means no expire,
// but it can be evicted when cache is full.
// if expireSeconds < 0 and if the key already exists, it will use the existing expireSeconds
// if expireSeconds == 0, then key won't have expire time
// if expireSeconds > 0, then the key will expire after expireSeconds
func (cache *Cache) Set(key, value []byte, expireSeconds int) (err error) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	err = cache.segments[segID].set(key, value, hashVal, expireSeconds)
	cache.locks[segID].Unlock()
	return
}

// Get returns the value or not found error.
func (cache *Cache) Get(key []byte) (value []byte, err error) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	value, _, err = cache.segments[segID].get(key, hashVal)
	cache.locks[segID].Unlock()
	return
}

// GetWithExpiration returns the value with expiration or not found error.
func (cache *Cache) GetWithExpiration(key []byte) (value []byte, expireAt uint32, err error) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	value, expireAt, err = cache.segments[segID].get(key, hashVal)
	cache.locks[segID].Unlock()
	return
}

// TTL returns the TTL time left for a given key or a not found error.
func (cache *Cache) TTL(key []byte) (timeLeft uint32, err error) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	timeLeft, err = cache.segments[segID].ttl(key, hashVal)
	cache.locks[segID].Unlock()
	return
}

// Del deletes an item in the cache by key and returns true or false if a delete occurred.
func (cache *Cache) Del(key []byte) (affected bool) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	affected = cache.segments[segID].del(key, hashVal)
	cache.locks[segID].Unlock()
	return
}

// SetInt stores in integer value in the cache.
func (cache *Cache) SetInt(key int64, value []byte, expireSeconds int) (err error) {
	var bKey [8]byte
	binary.LittleEndian.PutUint64(bKey[:], uint64(key))
	return cache.Set(bKey[:], value, expireSeconds)
}

// GetInt returns the value for an integer within the cache or a not found error.
func (cache *Cache) GetInt(key int64) (value []byte, err error) {
	var bKey [8]byte
	binary.LittleEndian.PutUint64(bKey[:], uint64(key))
	return cache.Get(bKey[:])
}

// GetIntWithExpiration returns the value and expiration or a not found error.
func (cache *Cache) GetIntWithExpiration(key int64) (value []byte, expireAt uint32, err error) {
	var bKey [8]byte
	binary.LittleEndian.PutUint64(bKey[:], uint64(key))
	return cache.GetWithExpiration(bKey[:])
}

// DelInt deletes an item in the cache by int key and returns true or false if a delete occurred.
func (cache *Cache) DelInt(key int64) (affected bool) {
	var bKey [8]byte
	binary.LittleEndian.PutUint64(bKey[:], uint64(key))
	return cache.Del(bKey[:])
}

func incrValue(valueBytes []byte, valueType string) (interface{}, []byte, error) {
	switch valueType {
	case "UINT64":
		valueBytes64 := [8]byte{}
		valueUInt64 := binary.LittleEndian.Uint64(valueBytes)
		valueUInt64++
		binary.LittleEndian.PutUint64(valueBytes64[:], valueUInt64)
		return valueUInt64, valueBytes64[:], nil
	case "UINT32":
		valueBytes32 := [4]byte{}
		valueUInt32 := binary.LittleEndian.Uint32(valueBytes)
		valueUInt32++
		binary.LittleEndian.PutUint32(valueBytes32[:], valueUInt32)
		return valueUInt32, valueBytes32[:], nil
	case "INT64":
		valueBytes64 := [8]byte{}
		valueInt64 := int64(binary.LittleEndian.Uint64(valueBytes))
		valueInt64++
		binary.LittleEndian.PutUint64(valueBytes64[:], uint64(valueInt64))
		return valueInt64, valueBytes64[:], nil
	case "INT32":
		valueBytes32 := [4]byte{}
		valueInt32 := int32(binary.LittleEndian.Uint32(valueBytes))
		valueInt32++
		binary.LittleEndian.PutUint32(valueBytes32[:], uint32(valueInt32))
		return valueInt32, valueBytes32[:], nil
	default:
		return nil, nil, errors.New("Type Not Supported")
	}

	return nil, valueBytes, nil
}

func newValueBytes(valueType string) []byte {
	switch valueType {
	case "INT64", "UINT64":
		valueBytes64 := [8]byte{}
		valueUInt64 := uint64(0)
		binary.LittleEndian.PutUint64(valueBytes64[:], valueUInt64)
		return valueBytes64[:]
	case "INT32", "UINT32":
		valueBytes32 := [4]byte{}
		valueUInt32 := uint32(0)
		binary.LittleEndian.PutUint32(valueBytes32[:], valueUInt32)
		return valueBytes32[:]
	default:
		return nil
	}
}

func (cache *Cache) SetValueInt(key []byte, value interface{}, expireSeconds int) error {
	switch value.(type) {
	case int64, uint64:
		valueBytes64 := [8]byte{}
		binary.LittleEndian.PutUint64(valueBytes64[:], value.(uint64))
		return cache.Set(key, valueBytes64[:], expireSeconds)
	case int32, uint32:
		valueBytes32 := [4]byte{}
		binary.LittleEndian.PutUint32(valueBytes32[:], value.(uint32))
		return cache.Set(key, valueBytes32[:], expireSeconds)
	default:
		return errors.New("type not supported")
	}
}

/*
 * Caller should convert the return value.
 * e.g.: valueInt32 := cache.GetValueInt(key).(int32)
 */
func (cache *Cache) GetValueInt(key []byte) (interface{}, error) {
	value, err := cache.Get(key)

	if err != nil {
		return nil, err
	}

	switch len(value) {
	case 8:
		return binary.LittleEndian.Uint64(value), nil
	case 4:
		return binary.LittleEndian.Uint32(value), nil
	default:
		return nil, errors.New("unexpected value")
	}
}

// Increments the value, assuming the value is an integer.
// Type of the integer is mentioned in valueType param, which could be any of {"UINT64", "UINT32", "INT64", "INT32"}
// returns in numeric type
// NOTE: expireSeconds will only be used if key doesn't exist. Else, old expireSeconds will be used.
func (cache *Cache) IncrValueInt(key []byte, valueType string, expireSeconds int) (interface{}, error) {
	hashVal := hashFunc(key)
	segID := hashVal & segmentAndOpVal
	cache.locks[segID].Lock()
	defer cache.locks[segID].Unlock()

	valueBytes, _, err := cache.segments[segID].get(key, hashVal)
	if err == ErrNotFound {
		// key doesn't exist. Create a new value with 0 as value.
		valueBytes = newValueBytes(valueType)
	} else if err != nil {
		return nil, err
	} else {
		// the key already exists, and so use the existing expireSeconds instead of the current one.
		expireSeconds = -1
	}

	valueNum, valueBytes, err := incrValue(valueBytes, valueType)
	if err != nil {
		return nil, err
	}

	err = cache.segments[segID].set(key, valueBytes, hashVal, expireSeconds)
	return valueNum, err
}

// EvacuateCount is a metric indicating the number of times an eviction occurred.
func (cache *Cache) EvacuateCount() (count int64) {
	for i := range cache.segments {
		count += atomic.LoadInt64(&cache.segments[i].totalEvacuate)
	}
	return
}

// ExpiredCount is a metric indicating the number of times an expire occurred.
func (cache *Cache) ExpiredCount() (count int64) {
	for i := range cache.segments {
		count += atomic.LoadInt64(&cache.segments[i].totalExpired)
	}
	return
}

// EntryCount returns the number of items currently in the cache.
func (cache *Cache) EntryCount() (entryCount int64) {
	for i := range cache.segments {
		entryCount += atomic.LoadInt64(&cache.segments[i].entryCount)
	}
	return
}

// AverageAccessTime returns the average unix timestamp when a entry being accessed.
// Entries have greater access time will be evacuated when it
// is about to be overwritten by new value.
func (cache *Cache) AverageAccessTime() int64 {
	var entryCount, totalTime int64
	for i := range cache.segments {
		totalTime += atomic.LoadInt64(&cache.segments[i].totalTime)
		entryCount += atomic.LoadInt64(&cache.segments[i].totalCount)
	}
	if entryCount == 0 {
		return 0
	} else {
		return totalTime / entryCount
	}
}

// HitCount is a metric that returns number of times a key was found in the cache.
func (cache *Cache) HitCount() (count int64) {
	for i := range cache.segments {
		count += atomic.LoadInt64(&cache.segments[i].hitCount)
	}
	return
}

// MissCount is a metric that returns the number of times a miss occurred in the cache.
func (cache *Cache) MissCount() (count int64) {
	for i := range cache.segments {
		count += atomic.LoadInt64(&cache.segments[i].missCount)
	}
	return
}

// LookupCount is a metric that returns the number of times a lookup for a given key occurred.
func (cache *Cache) LookupCount() int64 {
	return cache.HitCount() + cache.MissCount()
}

// HitRate is the ratio of hits over lookups.
func (cache *Cache) HitRate() float64 {
	hitCount, missCount := cache.HitCount(), cache.MissCount()
	lookupCount := hitCount + missCount
	if lookupCount == 0 {
		return 0
	} else {
		return float64(hitCount) / float64(lookupCount)
	}
}

// OverwriteCount indicates the number of times entries have been overriden.
func (cache *Cache) OverwriteCount() (overwriteCount int64) {
	for i := range cache.segments {
		overwriteCount += atomic.LoadInt64(&cache.segments[i].overwrites)
	}
	return
}

func (cache *Cache) SlotsDataExpandCount() (expandCount uint64) {
	for i := range cache.segments {
		expandCount += atomic.LoadUint64(&cache.segments[i].nSDExpands)
	}
	return
}

func (cache *Cache) SetCount() (setCount uint64) {
	for i := range cache.segments {
		setCount += atomic.LoadUint64(&cache.segments[i].nSets)
	}
	return
}

func (cache *Cache) GetCount() (getCount uint64) {
	for i := range cache.segments {
		getCount += uint64(atomic.LoadInt64(&cache.segments[i].hitCount)) +
			uint64(atomic.LoadInt64(&cache.segments[i].missCount))
	}
	return
}

func (cache *Cache) TotalSetTimeNs() (totalSetTimeNs uint64) {
	for i := range cache.segments {
		totalSetTimeNs += atomic.LoadUint64(&cache.segments[i].totalSetTimeNs)
	}
	return
}

func (cache *Cache) TotalGetTimeNs() (totalGetTimeNs uint64) {
	for i := range cache.segments {
		totalGetTimeNs += atomic.LoadUint64(&cache.segments[i].totalGetTimeNs)
	}
	return
}

func (cache *Cache) TotalEvacuateTimeNs() (totalEvacuateTimeNs uint64) {
	for i := range cache.segments {
		totalEvacuateTimeNs += atomic.LoadUint64(&cache.segments[i].totalEvacuateTimeNs)
	}
	return
}

func (cache *Cache) TotalSlotsDataExpandTimeNs() (totalSDExpandTimeNs uint64) {
	for i := range cache.segments {
		totalSDExpandTimeNs += atomic.LoadUint64(&cache.segments[i].totalSDExpandTimeNs)
	}
	return
}

func (cache *Cache) SlotsDataMemInUse() (sdMemInUse uint64) {
	for i := range cache.segments {
		sdMemInUse += atomic.LoadUint64(&cache.segments[i].sdMemInUse)
	}
	return
}

func (cache *Cache) SegmentsCount() (int) {
	return len(cache.segments)
}

func (cache *Cache) GetSegmentStats(index int) (m *map[string]uint64) {
	if index >= len(cache.segments) {
		fmt.Errorf("Out of boundary cache segment access. Index: %d", index)
		return nil
	}

	(*m)["segment" + string(index) + "nEvacuates"] = uint64(atomic.LoadInt64(&cache.segments[index].totalEvacuate))
	(*m)["segment" + string(index) + "nExpires"] = uint64(atomic.LoadInt64(&cache.segments[index].totalExpired))
	(*m)["segment" + string(index) + "nEntries"] = uint64(atomic.LoadInt64(&cache.segments[index].entryCount))
	(*m)["segment" + string(index) + "nSlotsDataExpands"] = uint64(atomic.LoadUint64(&cache.segments[index].nSDExpands))
	(*m)["segment" + string(index) + "nSets"] = uint64(atomic.LoadUint64(&cache.segments[index].nSets))
	(*m)["segment" + string(index) + "nGets"] = uint64(atomic.LoadInt64(&cache.segments[index].hitCount)) +
							uint64(atomic.LoadInt64(&cache.segments[index].missCount))
	(*m)["segment" + string(index) + "totalSetTimeNS"] = uint64(atomic.LoadUint64(&cache.segments[index].totalSetTimeNs))
	(*m)["segment" + string(index) + "totalGetTimeNS"] = uint64(atomic.LoadUint64(&cache.segments[index].totalGetTimeNs))
	(*m)["segment" + string(index) + "totalEvacuateTimeNs"] = uint64(atomic.LoadUint64(&cache.segments[index].totalEvacuateTimeNs))
	(*m)["segment" + string(index) + "slotsDataMemInUse"] = uint64(atomic.LoadUint64(&cache.segments[index].sdMemInUse))
	(*m)["segment" + string(index) + "slotsDataMemReleasedToGC"] = uint64(atomic.LoadUint64(&cache.segments[index].totalSDMemReleasedToGC))

	totalTime := atomic.LoadInt64(&cache.segments[index].totalTime)
	entryCount := atomic.LoadInt64(&cache.segments[index].totalCount)

	if entryCount == 0 {
		(*m)["segment" + string(index) + "avgAccessTime"] = 0
	} else {
		(*m)["segment" + string(index) + "avgAccessTime"] = uint64(totalTime / entryCount)
	}

	return
}

// May also include memory that have not been freed by GC yet.
func (cache *Cache) TotalSlotsDataMemReleasedToGC() (totalSDMemReleasedToGC uint64) {
	for i := range cache.segments {
		totalSDMemReleasedToGC += atomic.LoadUint64(&cache.segments[i].totalSDMemReleasedToGC)
	}
	return
}

// Clear clears the cache.
func (cache *Cache) Clear() {
	for i := range cache.segments {
		cache.locks[i].Lock()
		cache.segments[i].clear()
		cache.locks[i].Unlock()
	}
}

// ResetStatistics refreshes the current state of the statistics.
func (cache *Cache) ResetStatistics() {
	for i := range cache.segments {
		cache.locks[i].Lock()
		cache.segments[i].resetStatistics()
		cache.locks[i].Unlock()
	}
}
