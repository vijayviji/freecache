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
to store list of tuples (header, key, value). Ring buffer is used as a append log, whereas
SlotsData is used as a table of pointers to entries in the ring buffer.

SlotsData is used to store list of list of (entryPointer) (it's a two dimensional
list constructed out of single dimensional list). The entryPointer array is divided into
256 slots (which are slices of entryPointer array). Each slot in-turn contains
list of entryPointer. The slot size (maximum number of entry pointers in a
 slot) is denoted in segment structure (slotCap). So,
all the slots in the SlotsData are of same size. No. of actual entryPointers
stored in a slot is denoted by slotLen array (one per segement).

Initially the slotCap is 1. Meaning, each slot can store only one entryPointer.
Then when we run out of space in a slot, we create a new SlotsData with
double capacity and copy the elements from old SlotsData, putting entryPointers
in proper slots.

As far as the ring buffer is concerned, the tuple (header, key, value) is
simply appended to the ring buffer.

For the segment ID, the first 8 LSB (Least Significat Bits) are used, and for
the slot ID, next 8 LSB ( or 8 MSB, not sure) are used.
SegmentID = hashVal & 256
SlotID = uint8(hashVal >> 8)) (Remove 8 LSB. Then do uint8 of hashVal. Will it give LSB or MSB?
Need to calculate)

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