package main

import (
	"bytes"
	"sync"
	"sync/atomic"
)

var config Config
var usageLogChan = make(chan UsageRecord, 5000)

var rrCounter atomic.Uint64

var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}
