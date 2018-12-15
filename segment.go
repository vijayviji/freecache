package freecache

import (
	"errors"
	"sync/atomic"
	"time"
	"unsafe"
)

const HASH_ENTRY_SIZE = 16
const ENTRY_HDR_SIZE = 24

var ErrLargeKey = errors.New("The key is larger than 65535")
var ErrLargeEntry = errors.New("The entry size is larger than 1/1024 of cache size")
var ErrNotFound = errors.New("Entry not found")

// entry pointer struct points to an entry in ring buffer
type entryPtr struct {
	offset   int64  // entry offset in ring buffer
	hash16   uint16 // entries are ordered by hash16 in a slot.
	keyLen   uint16 // used to compare a key
	reserved uint32
}

// entry header struct in ring buffer, followed by key and value.
type entryHdr struct {
	accessTime uint32
	expireAt   uint32
	keyLen     uint16
	hash16     uint16
	valLen     uint32
	valCap     uint32
	deleted    bool
	slotId     uint8
	reserved   uint16
}

// a segment contains 256 slots, a slot is an array of entry pointers ordered by hash16 value
// the entry can be looked up by hash value of the key.
type segment struct {
	rb    RingBuf // ring buffer that stores data
	segId int
	_     uint32

	// stats
	missCount     int64
	hitCount      int64
	entryCount    int64
	totalCount    int64 // number of entries in ring buffer, including deleted entries.
	totalTime     int64 // used to calculate least recent used entry.
	totalEvacuate int64 // total evacuates in this segment from last reset
	totalExpired  int64 // used for debug
	overwrites    int64 // used for debug

	// additional stats by Viji
	nSDExpands             uint64 // total slots data expands in this segment from last reset
	nSets                  uint64 // total sets in this segment from last reset
	totalSetTimeNs         uint64 // total time taken for sets in this segment in nanoseconds from last reset
	totalGetTimeNs         uint64 // total time taken for gets in this segment in nanoseconds from last reset
	totalEvacuateTimeNs        uint64 // total time taken for evacuate in this segment in nanoseconds from last reset
	totalSDExpandTimeNs    uint64 // total time taken for slotsData in this segment in nanoseconds from last reset
	sdMemInUse             uint64 // memory currently in use for slotsData
	totalSDMemReleasedToGC uint64 /* total memory (used by slotsData), which are released to GC from program start
	(may not have been claimed by GC yet) */

	vacuumLen int64      // up to vacuumLen, new data can be written without overwriting old data.
	slotLens  [256]int32 // The actual length for every slot.
	slotCap   int32      // max number of entry pointers a slot can hold.
	slotsData []entryPtr // shared by all 256 slots
}

func newSegment(bufSize int, segId int) (seg segment) {
	seg.rb = NewRingBuf(bufSize, 0)
	seg.segId = segId
	seg.vacuumLen = int64(bufSize)
	seg.slotCap = 1
	seg.slotsData = make([]entryPtr, 256*seg.slotCap)
	return
}

// If expireSeconds is <= 0, then we use existingExpireAt value. Similar for other conditions.
func calculateExpireAt(now uint32, expireSeconds int, existingExpireAt uint32) uint32 {
	var expireAt uint32

	switch true {
	case expireSeconds < 0:
		expireAt = existingExpireAt
	case expireSeconds == 0:
		expireAt = 0
	case expireSeconds > 0:
		expireAt = now + uint32(expireSeconds)
	}

	return expireAt
}

// if expireSeconds < 0 and if the key already exists, it will use the existing expireSeconds
// if expireSeconds == 0, then key won't have expire time
// if expireSeconds > 0, then the key will expire after expireSeconds
func (seg *segment) set(key, value []byte, hashVal uint64, expireSeconds int) (err error) {
	if len(key) > 65535 {
		return ErrLargeKey
	}
	maxKeyValLen := len(seg.rb.data)/4 - ENTRY_HDR_SIZE
	if len(key)+len(value) > maxKeyValLen {
		// Do not accept large entry.
		return ErrLargeEntry
	}

	defer atomic.AddUint64(&seg.nSets, 1)

	now := uint32(time.Now().Unix())

	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)

	var hdrBuf [ENTRY_HDR_SIZE]byte
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))

	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if match {
		matchedPtr := &slot[idx]
		seg.rb.ReadAt(hdrBuf[:], matchedPtr.offset)
		hdr.slotId = slotId
		hdr.hash16 = hash16
		hdr.keyLen = uint16(len(key))
		originAccessTime := hdr.accessTime
		hdr.accessTime = now
		hdr.expireAt = calculateExpireAt(now, expireSeconds, hdr.expireAt)
		hdr.valLen = uint32(len(value))
		if hdr.valCap >= hdr.valLen {
			//in place overwrite
			atomic.AddUint64(&seg.totalSetTimeNs, uint64(time.Now().Unix())-uint64(now))
			atomic.AddInt64(&seg.totalTime, int64(hdr.accessTime)-int64(originAccessTime))
			seg.rb.WriteAt(hdrBuf[:], matchedPtr.offset)
			seg.rb.WriteAt(value, matchedPtr.offset+ENTRY_HDR_SIZE+int64(hdr.keyLen))
			atomic.AddInt64(&seg.overwrites, 1)
			return
		}
		// avoid unnecessary memory copy.
		seg.delEntryPtr(slotId, hash16, slot[idx].offset)
		//seg.delEntryPtr(slotId, hash16, seg.slotsData[idx].offset)
		match = false
		// increase capacity and limit entry len.
		for hdr.valCap < hdr.valLen {
			hdr.valCap *= 2
		}
		if hdr.valCap > uint32(maxKeyValLen-len(key)) {
			hdr.valCap = uint32(maxKeyValLen - len(key))
		}
	} else {
		hdr.slotId = slotId
		hdr.hash16 = hash16
		hdr.keyLen = uint16(len(key))
		hdr.accessTime = now
		hdr.expireAt = calculateExpireAt(now, expireSeconds, 0)
		hdr.valLen = uint32(len(value))
		hdr.valCap = uint32(len(value))
		if hdr.valCap == 0 { // avoid infinite loop when increasing capacity.
			hdr.valCap = 1
		}
	}

	entryLen := ENTRY_HDR_SIZE + int64(len(key)) + int64(hdr.valCap)
	slotModified := seg.evacuate(entryLen, slotId, now)
	if slotModified {
		// the slot has been modified during evacuation, we need to looked up for the 'idx' again.
		// otherwise there would be index out of bound error.
		slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
		idx, match = seg.lookup(slot, hash16, key)
		// assert(match == false)
	}
	newOff := seg.rb.End()
	seg.insertEntryPtr(slotId, hash16, newOff, idx, hdr.keyLen)
	seg.rb.Write(hdrBuf[:])
	seg.rb.Write(key)
	seg.rb.Write(value)
	seg.rb.Skip(int64(hdr.valCap - hdr.valLen))
	atomic.AddUint64(&seg.totalSetTimeNs, uint64(time.Now().Unix())-uint64(now))
	atomic.AddInt64(&seg.totalTime, int64(now))
	atomic.AddInt64(&seg.totalCount, 1)
	seg.vacuumLen -= entryLen
	return
}

func (seg *segment) evacuate(entryLen int64, slotId uint8, now uint32) (slotModified bool) {
	var oldHdrBuf [ENTRY_HDR_SIZE]byte
	consecutiveEvacuate := 0

	evacuationOccurred := false
	timeBegin := uint64(time.Now().Unix())
	for seg.vacuumLen < entryLen {
		oldOff := seg.rb.End() + seg.vacuumLen - seg.rb.Size()
		seg.rb.ReadAt(oldHdrBuf[:], oldOff)
		oldHdr := (*entryHdr)(unsafe.Pointer(&oldHdrBuf[0]))
		oldEntryLen := ENTRY_HDR_SIZE + int64(oldHdr.keyLen) + int64(oldHdr.valCap)
		if oldHdr.deleted {
			consecutiveEvacuate = 0
			atomic.AddInt64(&seg.totalTime, -int64(oldHdr.accessTime))
			atomic.AddInt64(&seg.totalCount, -1)
			seg.vacuumLen += oldEntryLen
			continue
		}
		expired := oldHdr.expireAt != 0 && oldHdr.expireAt < now
		leastRecentUsed := int64(oldHdr.accessTime)*atomic.LoadInt64(&seg.totalCount) <= atomic.LoadInt64(&seg.totalTime)
		if expired || leastRecentUsed || consecutiveEvacuate > 5 {
			seg.delEntryPtr(oldHdr.slotId, oldHdr.hash16, oldOff)
			if oldHdr.slotId == slotId {
				slotModified = true
			}
			consecutiveEvacuate = 0
			atomic.AddInt64(&seg.totalTime, -int64(oldHdr.accessTime))
			atomic.AddInt64(&seg.totalCount, -1)
			seg.vacuumLen += oldEntryLen
			if expired {
				atomic.AddInt64(&seg.totalExpired, 1)
			}
		} else {
			// evacuate an old entry that has been accessed recently for better cache hit rate.
			newOff := seg.rb.Evacuate(oldOff, int(oldEntryLen))
			seg.updateEntryPtr(oldHdr.slotId, oldHdr.hash16, oldOff, newOff)
			consecutiveEvacuate++
			atomic.AddInt64(&seg.totalEvacuate, 1)
		}
		evacuationOccurred = true
	}

	if evacuationOccurred {
		atomic.AddUint64(&seg.totalEvacuateTimeNs, uint64(time.Now().Unix())-timeBegin)
	}

	return
}

func (seg *segment) get(key []byte, hashVal uint64) (value []byte, expireAt uint32, err error) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	slotOff := int32(slotId) * seg.slotCap
	var slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		err = ErrNotFound
		atomic.AddInt64(&seg.missCount, 1)
		return
	}
	ptr := &slot[idx]
	now := uint32(time.Now().Unix())

	var hdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(hdrBuf[:], ptr.offset)
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))
	expireAt = hdr.expireAt

	if hdr.expireAt != 0 && hdr.expireAt <= now {
		seg.delEntryPtr(slotId, hash16, ptr.offset)
		atomic.AddInt64(&seg.totalExpired, 1)
		err = ErrNotFound
		atomic.AddInt64(&seg.missCount, 1)
		atomic.AddUint64(&seg.totalGetTimeNs, uint64(time.Now().Unix())-uint64(now))
		return
	}
	atomic.AddInt64(&seg.totalTime, int64(now-hdr.accessTime))
	hdr.accessTime = now
	seg.rb.WriteAt(hdrBuf[:], ptr.offset)
	value = make([]byte, hdr.valLen)

	seg.rb.ReadAt(value, ptr.offset+ENTRY_HDR_SIZE+int64(hdr.keyLen))
	atomic.AddInt64(&seg.hitCount, 1)
	atomic.AddUint64(&seg.totalGetTimeNs, uint64(time.Now().Unix())-uint64(now))
	return
}

func (seg *segment) del(key []byte, hashVal uint64) (affected bool) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		return false
	}
	ptr := &slot[idx]
	seg.delEntryPtr(slotId, hash16, ptr.offset)
	return true
}

func (seg *segment) ttl(key []byte, hashVal uint64) (timeLeft uint32, err error) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	slotOff := int32(slotId) * seg.slotCap
	var slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		err = ErrNotFound
		return
	}
	ptr := &slot[idx]
	now := uint32(time.Now().Unix())

	var hdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(hdrBuf[:], ptr.offset)
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))

	if hdr.expireAt == 0 {
		timeLeft = 0
		return
	} else if hdr.expireAt != 0 && hdr.expireAt >= now {
		timeLeft = hdr.expireAt - now
		return
	}
	err = ErrNotFound
	return
}

func (seg *segment) expand() {
	defer atomic.AddUint64(&seg.nSDExpands, 1)

	timeBegin := uint64(time.Now().Unix())
	newSlotData := make([]entryPtr, seg.slotCap*2*256)
	for i := 0; i < 256; i++ {
		off := int32(i) * seg.slotCap
		copy(newSlotData[off*2:], seg.slotsData[off:off+seg.slotLens[i]])
	}
	atomic.AddUint64(&seg.totalSDMemReleasedToGC, uint64(unsafe.Sizeof(seg.slotsData[0]))*uint64(seg.slotCap)*256)

	seg.slotCap *= 2
	seg.slotsData = newSlotData

	atomic.StoreUint64(&seg.sdMemInUse, uint64(unsafe.Sizeof(seg.slotsData[0]))*uint64(seg.slotCap)*256)
	atomic.AddUint64(&seg.totalSDExpandTimeNs, uint64(time.Now().Unix())-uint64(timeBegin))
}

func (seg *segment) updateEntryPtr(slotId uint8, hash16 uint16, oldOff, newOff int64) {
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookupByOff(slot, hash16, oldOff)
	if !match {
		return
	}
	ptr := &slot[idx]
	ptr.offset = newOff
}

func (seg *segment) insertEntryPtr(slotId uint8, hash16 uint16, offset int64, idx int, keyLen uint16) {
	slotOff := int32(slotId) * seg.slotCap
	if seg.slotLens[slotId] == seg.slotCap {
		seg.expand()
		slotOff *= 2
	}
	seg.slotLens[slotId]++
	atomic.AddInt64(&seg.entryCount, 1)
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	copy(slot[idx+1:], slot[idx:])
	slot[idx].offset = offset
	slot[idx].hash16 = hash16
	slot[idx].keyLen = keyLen
}

func (seg *segment) delEntryPtr(slotId uint8, hash16 uint16, offset int64) {
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookupByOff(slot, hash16, offset)
	if !match {
		return
	}
	var entryHdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(entryHdrBuf[:], offset)
	entryHdr := (*entryHdr)(unsafe.Pointer(&entryHdrBuf[0]))
	entryHdr.deleted = true
	seg.rb.WriteAt(entryHdrBuf[:], offset)
	copy(slot[idx:], slot[idx+1:])
	seg.slotLens[slotId]--
	atomic.AddInt64(&seg.entryCount, -1)
}

func entryPtrIdx(slot []entryPtr, hash16 uint16) (idx int) {
	high := len(slot)
	for idx < high {
		mid := (idx + high) >> 1
		oldEntry := &slot[mid]
		if oldEntry.hash16 < hash16 {
			idx = mid + 1
		} else {
			high = mid
		}
	}
	return
}

func (seg *segment) lookup(slot []entryPtr, hash16 uint16, key []byte) (idx int, match bool) {
	idx = entryPtrIdx(slot, hash16)
	for idx < len(slot) {
		ptr := &slot[idx]
		if ptr.hash16 != hash16 {
			break
		}
		match = int(ptr.keyLen) == len(key) && seg.rb.EqualAt(key, ptr.offset+ENTRY_HDR_SIZE)
		if match {
			return
		}
		idx++
	}
	return
}

func (seg *segment) lookupByOff(slot []entryPtr, hash16 uint16, offset int64) (idx int, match bool) {
	idx = entryPtrIdx(slot, hash16)
	for idx < len(slot) {
		ptr := &slot[idx]
		if ptr.hash16 != hash16 {
			break
		}
		match = ptr.offset == offset
		if match {
			return
		}
		idx++
	}
	return
}

func (seg *segment) resetStatistics() {
	atomic.StoreInt64(&seg.totalEvacuate, 0)
	atomic.StoreInt64(&seg.totalExpired, 0)
	atomic.StoreInt64(&seg.overwrites, 0)
	atomic.StoreInt64(&seg.hitCount, 0)
	atomic.StoreInt64(&seg.missCount, 0)

	atomic.StoreUint64(&seg.nSDExpands, 0)
	atomic.StoreUint64(&seg.nSets, 0)
	atomic.StoreUint64(&seg.totalSetTimeNs, 0)
	atomic.StoreUint64(&seg.totalGetTimeNs, 0)
	atomic.StoreUint64(&seg.totalEvacuateTimeNs, 0)
	atomic.StoreUint64(&seg.totalSDExpandTimeNs, 0)
}

func (seg *segment) clear() {
	bufSize := len(seg.rb.data)
	seg.rb = NewRingBuf(bufSize, 0)
	seg.vacuumLen = int64(bufSize)
	seg.slotCap = 1
	seg.slotsData = make([]entryPtr, 256*seg.slotCap)
	for i := 0; i < len(seg.slotLens); i++ {
		seg.slotLens[i] = 0
	}

	atomic.StoreInt64(&seg.entryCount, 0)
	atomic.StoreInt64(&seg.totalCount, 0)
	atomic.StoreInt64(&seg.totalTime, 0)

	seg.resetStatistics()
}
