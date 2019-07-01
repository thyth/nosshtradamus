package predictive

import (
	"io"
	"runtime"
	"sync"
	"time"
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
	mutex       *sync.Mutex
	buffer      []byte
	bufferIndex int

	writeNotify chan interface{}
	upstreamErr error
}

func MakeAsynk(upstream io.Writer, capacity int) *Asynk {
	asynk := &Asynk{
		upstream:    upstream,
		mutex:       &sync.Mutex{},
		buffer:      make([]byte, capacity),
		bufferIndex: 0,

		writeNotify: make(chan interface{}),
	}
	go func(asynk *Asynk) {
		lastTransmittedIndex := 0
		for range asynk.writeNotify {
			asynk.mutex.Lock()
			nextIndex := asynk.bufferIndex
			asynk.mutex.Unlock()
			_, asynk.upstreamErr = upstream.Write(asynk.buffer[lastTransmittedIndex:nextIndex])
			if asynk.upstreamErr != nil {
				return
			}
			// if another asynk write happened while finishing the upstream write, we should have another notification
		}
	}(asynk)
	return asynk
}

func (asynk *Asynk) Close() error {
	close(asynk.writeNotify)
	if closer, ok := asynk.upstream.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (asynk *Asynk) Write(p []byte) (int, error) {
	if asynk.upstreamErr != nil {
		return 0, asynk.upstreamErr
	}
	asynk.mutex.Lock()
	n := copy(asynk.buffer[asynk.bufferIndex:], p)
	asynk.bufferIndex += n
	asynk.mutex.Unlock()

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
			// unfortunately we still have more data to write, so need to sleep and try again
			time.Sleep(1 * time.Millisecond)
			return asynk.Write(p[n:])
		} else {
			// we wrote everything we care about to the buffer, so can return and let the asynk deal with the upstream
			return n, nil
		}
	}
}
