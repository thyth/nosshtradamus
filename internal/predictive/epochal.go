package predictive

import (
	"io"
	"sync/atomic"
	"time"
)

// See description in https://gitlab.hive.thyth.com/chronostruct/nosshtradamus/issues/1 for epochs are needed.

type Epochal struct {
	upstream io.ReadWriteCloser
	epoch    *uint64

	requestGenerator func(epochal *Epochal, epoch uint64)
	epochChanged     func(epoch uint64, pending bool, latency time.Duration)
}

func MakeEpochal(rwc io.ReadWriteCloser, requestGenerator func(*Epochal, uint64), onEpochIncrement func(uint64, bool, time.Duration)) *Epochal {
	startingEpoch := uint64(0)
	return &Epochal{
		upstream: rwc,
		epoch: &startingEpoch,

		requestGenerator: requestGenerator,
		epochChanged:     onEpochIncrement,
	}
}

func (e *Epochal) Read(p []byte) (int, error) {
	return e.upstream.Read(p)
}

func (e *Epochal) Write(p []byte) (int, error) {
	n, err := e.upstream.Write(p)
	if err == nil && e.requestGenerator != nil {
		requestedEpoch := atomic.AddUint64(e.epoch, 1)
		e.requestGenerator(e, requestedEpoch)
	}
	return n, err
}

func (e *Epochal) Close() error {
	return e.upstream.Close()
}

func (e *Epochal) ResponseTo(epoch uint64, pingTime time.Time) bool {
	// variant A -- just pass it through
	pending := atomic.LoadUint64(e.epoch) > epoch
	e.epochChanged(epoch, pending, time.Now().Sub(pingTime))
	return pending
}
