/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2019-2024 Daniel Selifonov
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package predictive

import (
	"container/heap"
	"io"
	"runtime"
	"sync"
)

// WriteWithNotify extends io.Writer with WriteNotify, which will invoke the onWritten callback after the payload is
// completely written.
type WriteWithNotify interface {
	WriteNotify(p []byte, onWritten func()) (int, error)
}

// Asynk - Asynchronous Sink Writer
//
// An asynk implements the io.WriterCloser interface wrapping another io.Writer; writes to the asynk (within available
// buffer capacity) will return immediately, even if the underlying writer blocks. If buffer capacity is exceeded,
// however, the asynk will block until the underlying writer starts to clear.
//
// Calling close will propagate to the underlying io.Writer if it also implements io.Closer; otherwise it will just stop
// the asynk.
type Asynk struct {
	upstream    io.Writer
	cond        *sync.Cond
	buffer      []byte
	bufferIndex int

	prospectiveWrittenBytes uint64
	callbacks               callbackPriorityQueue

	writeNotify chan any
	upstreamErr error
}

func MakeAsynk(upstream io.Writer, capacity int) *Asynk {
	asw := &Asynk{
		upstream:    upstream,
		cond:        sync.NewCond(&sync.Mutex{}),
		buffer:      make([]byte, capacity),
		bufferIndex: 0,

		prospectiveWrittenBytes: 0,
		callbacks:               callbackPriorityQueue{},

		writeNotify: make(chan any, 1), // buffer up to one notification, for notifying during a write
	}
	go func() {
		defer func() {
			_ = asw.Close()
		}()
		lastTransmittedIndex := 0
		totalWrittenBytes := uint64(0)
		for range asw.writeNotify {
			asw.cond.L.Lock()
			nextIndex := asw.bufferIndex
			asw.cond.L.Unlock()
			var writtenBytes int
			writtenBytes, asw.upstreamErr = upstream.Write(asw.buffer[lastTransmittedIndex:nextIndex])
			lastTransmittedIndex += writtenBytes
			totalWrittenBytes += uint64(writtenBytes)
			if asw.upstreamErr != nil {
				return
			}
			asw.cond.L.Lock()
			// check for callback execution
			if len(asw.callbacks) != 0 {
				front := asw.callbacks[0] // peek first, since we might not pop the item
				for front != nil && front.index <= totalWrittenBytes {
					// consume the item
					front.callback()
					heap.Pop(&asw.callbacks)
					// re-peek for the next possible iteration
					if asw.callbacks.Len() > 0 {
						front = asw.callbacks[0]
					} else {
						front = nil
					}
				}
			}

			// if we've written the entire buffer, reset the index to reclaim usable capacity
			postWriteIndex := asw.bufferIndex
			if postWriteIndex == nextIndex {
				asw.bufferIndex = 0
				lastTransmittedIndex = 0
			}
			asw.cond.Signal()
			asw.cond.L.Unlock()
			// if another asynk write happened while finishing the upstream write, we should have another notification
		}
	}()
	return asw
}

func (asw *Asynk) Close() error {
	if asw.upstreamErr == nil {
		asw.upstreamErr = io.EOF
	}
	close(asw.writeNotify)
	asw.cond.Broadcast() // release any client waiting for space to write
	if closer, ok := asw.upstream.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (asw *Asynk) Write(p []byte) (int, error) {
	return asw.WriteNotify(p, nil)
}

// WriteNotify takes a payload and a callback executed once that payload is actually written to the underlying
// io.Writer instance. The onWritten callback must have very low execution duration.
func (asw *Asynk) WriteNotify(p []byte, onWritten func()) (int, error) {
recur:
	for {
		if asw.upstreamErr != nil {
			return 0, asw.upstreamErr
		}
		asw.cond.L.Lock()
		if onWritten != nil {
			callbackTargetIndex := asw.prospectiveWrittenBytes + uint64(len(p))
			queueItem := &indexedCallback{
				index:    callbackTargetIndex,
				callback: onWritten,
			}
			heap.Push(&asw.callbacks, queueItem)
			onWritten = nil
		}
		n := copy(asw.buffer[asw.bufferIndex:], p)
		asw.bufferIndex += n
		asw.prospectiveWrittenBytes += uint64(n)
		asw.cond.L.Unlock()

		select {
		case asw.writeNotify <- true:
			// write was put -- check if we pushed everything
			if len(p) > n {
				// didn't fit in the buffer -- try to write the remaining
				runtime.Gosched()
				p = p[n:] // avoid true recursion, since there is no TCO
				continue recur
			} else {
				// everything was written too -- we're done
				return n, nil
			}
		default:
			// put was rejected -- upstream must be slow
			if len(p) > n {
				// unfortunately we still have more data to write, so need to wait for room and try again
				asw.cond.L.Lock()
				asw.cond.Wait()
				asw.cond.L.Unlock()
				p = p[n:] // avoid true recursion, since there is no TCO
				continue recur
			} else {
				// we wrote everything we care about to the buffer, so can return and let the asynk deal with the upstream
				return n, nil
			}
		}
	}
}

type indexedCallback struct {
	index    uint64
	callback func()
}

type callbackPriorityQueue []*indexedCallback

//goland:noinspection GoMixedReceiverTypes
func (cpq callbackPriorityQueue) Len() int { return len(cpq) }

//goland:noinspection GoMixedReceiverTypes
func (cpq callbackPriorityQueue) Less(i, j int) bool { return cpq[i].index < cpq[j].index }

//goland:noinspection GoMixedReceiverTypes
func (cpq callbackPriorityQueue) Swap(i, j int) { cpq[i], cpq[j] = cpq[j], cpq[i] }

//goland:noinspection GoMixedReceiverTypes
func (cpq *callbackPriorityQueue) Push(x any) {
	*cpq = append(*cpq, x.(*indexedCallback))
}

//goland:noinspection GoMixedReceiverTypes
func (cpq *callbackPriorityQueue) Pop() any {
	old := *cpq
	n := len(old)
	x := old[n-1]
	*cpq = old[:n-1]
	return x
}
