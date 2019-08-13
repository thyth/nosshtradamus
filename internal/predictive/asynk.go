/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2019 Daniel Selifonov
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
	"io"
	"runtime"
	"sync"
)

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

	writeNotify chan interface{}
	upstreamErr error
}

func MakeAsynk(upstream io.Writer, capacity int) *Asynk {
	asynk := &Asynk{
		upstream:    upstream,
		cond:        sync.NewCond(&sync.Mutex{}),
		buffer:      make([]byte, capacity),
		bufferIndex: 0,

		writeNotify: make(chan interface{}, 1), // buffer up to one notification, for notifying during a write
	}
	go func(asynk *Asynk) {
		lastTransmittedIndex := 0
		for range asynk.writeNotify {
			asynk.cond.L.Lock()
			nextIndex := asynk.bufferIndex
			asynk.cond.L.Unlock()
			_, asynk.upstreamErr = upstream.Write(asynk.buffer[lastTransmittedIndex:nextIndex])
			lastTransmittedIndex = nextIndex
			if asynk.upstreamErr != nil {
				return
			}
			asynk.cond.L.Lock()
			// if we've written the entire buffer, reset the index to reclaim usable capacity
			postWriteIndex := asynk.bufferIndex
			if postWriteIndex == nextIndex {
				asynk.bufferIndex = 0
				lastTransmittedIndex = 0
			}
			asynk.cond.Signal()
			asynk.cond.L.Unlock()
			// if another asynk write happened while finishing the upstream write, we should have another notification
		}
	}(asynk)
	return asynk
}

func (asynk *Asynk) Close() error {
	if asynk.upstreamErr == nil {
		asynk.upstreamErr = io.EOF
	}
	close(asynk.writeNotify)
	asynk.cond.Broadcast() // release any client waiting for space to write
	if closer, ok := asynk.upstream.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (asynk *Asynk) Write(p []byte) (int, error) {
	if asynk.upstreamErr != nil {
		return 0, asynk.upstreamErr
	}
	asynk.cond.L.Lock()
	n := copy(asynk.buffer[asynk.bufferIndex:], p)
	asynk.bufferIndex += n
	asynk.cond.L.Unlock()

	select {
	case asynk.writeNotify <- true:
		// write was put -- check if we pushed everything
		if len(p) > n {
			// didn't fit in the buffer -- try to write the remaining
			runtime.Gosched()
			return asynk.Write(p[n:])
		} else {
			// everything was written too -- we're done
			return n, nil
		}
	default:
		// put was rejected -- upstream must be slow
		if len(p) > n {
			// unfortunately we still have more data to write, so need to wait for room and try again
			asynk.cond.L.Lock()
			asynk.cond.Wait()
			asynk.cond.L.Unlock()
			return asynk.Write(p[n:])
		} else {
			// we wrote everything we care about to the buffer, so can return and let the asynk deal with the upstream
			return n, nil
		}
	}
}
