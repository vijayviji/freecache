# FreeCache - A cache library for Go with zero GC overhead and high concurrent performance.

Long lived objects in memory introduce expensive GC overhead, With FreeCache, you can cache unlimited number of objects in memory 
without increased latency and degraded throughput. 

[![Build Status](https://travis-ci.org/coocood/freecache.png?branch=master)](https://travis-ci.org/coocood/freecache)
[![GoCover](http://gocover.io/_badge/github.com/coocood/freecache)](http://gocover.io/github.com/coocood/freecache)
[![GoDoc](https://godoc.org/github.com/coocood/freecache?status.svg)](https://godoc.org/github.com/coocood/freecache)

## Features

* Store hundreds of millions of entries
* Zero GC overhead
* High concurrent thread-safe access
* Pure Go implementation
* Expiration support
* Nearly LRU algorithm
* Strictly limited memory usage
* Come with a toy server that supports a few basic Redis commands with pipeline
* Iterator support

## Performance

Here is the benchmark result compares to built-in map, `Set` performance is about 2x faster than built-in map, `Get` performance is about 1/2x slower than built-in map. Since it is single threaded benchmark, in multi-threaded environment, 
FreeCache should be many times faster than single lock protected built-in map.

    BenchmarkCacheSet        3000000               446 ns/op
    BenchmarkMapSet          2000000               861 ns/op
    BenchmarkCacheGet        3000000               517 ns/op
    BenchmarkMapGet         10000000               212 ns/op

## Example Usage

```go
cacheSize := 100 * 1024 * 1024
cache := freecache.NewCache(cacheSize)
debug.SetGCPercent(20)
key := []byte("abc")
val := []byte("def")
expire := 60 // expire in 60 seconds
cache.Set(key, val, expire)
got, err := cache.Get(key)
if err != nil {
    fmt.Println(err)
} else {
    fmt.Println(string(got))
}
affected := cache.Del(key)
fmt.Println("deleted key ", affected)
fmt.Println("entry count ", cache.EntryCount())
```

## Notice

* Memory is preallocated.
* If you allocate large amount of memory, you may need to set `debug.SetGCPercent()`
to a much lower percentage to get a normal GC frequency.

## How it is done

FreeCache avoids GC overhead by reducing the number of pointers.
No matter how many entries stored in it, there are only 512 pointers.
The data set is sharded into 256 segments by the hash value of the key.
Each segment has only two pointers, one is the ring buffer that stores keys and values, 
the other one is the index slice which used to lookup for an entry.
Each segment has its own lock, so it supports high concurrent access.

## Implementation Details (Author: Vijay)
### About Cache:
A new cache can be created by calling freecache.NewCache(cacheSize). This will
create new cache with 256 segements (aka buckets). Each segement will
contain a Ring Buffer and SlotsData (byte array). The ring buffer is used
to store list of tuples (header, key, value). Ring buffer is used as an append log, whereas
SlotsData is used as a table of pointers to entries in the ring buffer.
Think of this as similar to persistent hashtable, which would have on-disk buffer, and in-memory table
where in-memory table would give points to actual data in on-disk buffer. The ringbuffer is similar
to the on-disk buffer, and SlotsData is similar to in-memory table.

SlotsData is hashtable that has fixed length chaining. Single slot will have fixed size array to contain
entryPointer objects. The size of this array is stored in segment->slotCap. segment->slotLens\[i\] tells
how many actual elements are there in the array of slot i.

SlotsData is single byte array divided into 256 slots, with each slot having an array (actually slice)
of size segment->slotCap.

Initially the slotCap is 1. Meaning, each slot can store only one entryPointer.
Then when we run out of space in a slot, we create a new SlotsData with
double capacity and copy the elements from old SlotsData, putting entryPointers
in proper slots. This is table doubling method.

As far as the ring buffer is concerned, the tuple (header, key, value) is
simply appended to the ring buffer.

Key Calcualtion:
For the segment ID, the first 8 LSB (Least Significat Bits) are used, and for
the slot ID, next 8 LSB ( or 8 MSB, not sure) are used.
SegmentID = hashVal & 256
SlotID = uint8(hashVal >> 8)) (Remove 8 LSB. Then do uint8 of hashVal. Will it give LSB or MSB?
Need to calculate)

#### Data Structure:
##### Segment (unintuitive fields):
###### vacuumLen:
When the ringbuffer for the segment is not full, this shows how much more space we've. When the
ringbuffer is full, this is useless, however, when we evacuate elements from Segment (which is like
trim operation in disk), we add that space to vacuumLen. But ringBuffer isn't aware of this mapping.
So, ringbuffer is like a hardisk, and we have one more mapping (similar to filesystem) on top of ringbuffer.
This mapping is for knowing how much space is free in ringbuffer from segment point of view.

#### Operations:
##### Get()
##### Set()
Find the segment of the element and call set() of that segment. Then get the slotId for that hash in that
segment. Check if the hash is already there in the slot. If it's, then see if length of value of existing entry
is >= new value. If so, write the updated entry in the space in the slot and write the value in the space
pointed by the old entry in ring buffer. If len(value of existing entry) < len(value of new entry), then
delete that element in the slot. Then go for allocation.

If the hash is not there in the slot, then go for allocation.

Allocation: Try to insert this entry in the slot, pointed by slotID. If we don't have space, then
expand the slotsData table (Interestingly, this expansion is unbounded and that is ok, since we can't
store more data than ringbuffer allows. And slotsData only has entries that point to valid block in
ringbuffer. Others are deleted from slotsData anyways when we create space in ringbuffer during
evacuation). Then insert the entry in the slot. Then insert the data at the tail of the log (ringbuffer).

##### Incr()

##### Segment Operations:
###### set()
###### get()
###### evacuate(len, slotId, timeNow)
This actually frees space of size 'len' from ringbuffer. Note that, ringbuffer isn't aware of this.
Ringbuffer may still be full, but from segment's point of view, it has free space in ringbuffer.

Before evacuating, it checks if already we've enough space to accommodate entry of size 'len' and if so,
it returns. If not, it picks up the element at the (tail + free space mentioned by vacuumLen) in the ringbuffer.
Then if that element is deleted or expired or least recently used, then it goes ahead and deletes. If not,
it relogs that element in the ringbuffer (using rb.evacuate) and counts that space for free space and moves ahead with the
next element, until it collects all the space mentioned in 'len'. While relogging, we've to update the
SlotsData table as well.

The slotId, is used to check whether the element being deleted is in the slot pointed by 'slotId'.
timeNow is used to check for expired elements.

How it calculates if the element is LRU or not: It does an approximate calculation to avoid expensive
data structure in maintaining LRU. It takes the access time of the element and if we assume that
all the elements in the segment has this access time, and if it is less than the sum of the
access times of all the elements, then this is one of the least accessed elements. Why?
Let's say the access times are \[1, 2, 3, 4, 7, 9\]. The sum = (1+2+3+4+7+9) = 26, With this calculation
except 7 and 9, everything else will pass the approximate calculation here and could become LRU element.

### About RingBuffer:
Ringbuffer is a fixed log, where data gets appended, and when it goes beyond the siize of the ringbuffer,
it starts overwriting at the beginning of the log.

Ringbuffer uses tuple (index, begin, end). Begin and End are uint64 and are always
incrementing. Ideally these two should be enough to maintain the ringbuffer. (The max size
is len(dataBuffer), where dataBuffer is the actual byte array that holds data).


index says where the start of the ringbuffer in dataBuffer. index divides head and tail of the
log. The element prior to index is tail and the element at index is head. Elements at head gets overwritten first
when the ring buffer is full.

(Begin, End) are useful providing the
current length of the buffer, and len(dataBuffer) provides the maximum capacity.
If the ringbuffer is full, (Begin, End) and len(dataBuffer) does the same job.

#### Operations:
##### WriteAt(buf, offset):
It writes the buffer 'buf' at offset. This offset should be relative to (begin, end)
tuple. Suppose, I want to write the buffer 'buf' at 5th offset from the start of the buffer,
then offset should be ringbuffer.Begin() + 5.

##### ReadAt(buf, offset):
Same as WriteAt(). The result will be in 'buf'

##### Write(buf):
It appends the buffer 'buf' in the ringbuffer. Append always happens at the tail of the log.

##### Evacuate(off, len):
The idea of this operation is to relog the elements in the range (off, off+len). Relog means take the
elements and append at the tail of the log. (Remember, if the ringbuffer is full, elements at the head
of the log gets overwritten first, and the elements at the tail of the log gets overwritten last).

It achieves this by copying the data from range (off, off+len) and appends to the buffer. This way,
the data would become the last to be overwritten. (Internal implementation uses optimization to
avoid copies in some scenarios)

E.g.: Let's say this is the ringbuffer's dataBuffer \[11, 12, 13, 14, 15\]. Let's say the Ringbuffer object
is (Begin: 123, End: 128, Index: 3) (Begin - End = 5. Index is pointing to element '14' which means,
element '13' is at the tail. Let's say we want to relog (11, 12). The result will be
(dataBuffer: \[11, 12, 13, 11, 12\], Begin: 125, End: 130, Index: 0)

## TODO

* Support dump to file and load from file.
* Support resize cache size at runtime.

## License

The MIT License

## How to Test
`$> go test`

## How to run benchmark
`$> go test -bench .`