package predictive

import (
	"io"
	"time"
)

// Simple Wrapper for io.ReadWriteCloser instances which imposes the specified delay on both reads and writes.
type Delayer struct {
	upstream io.ReadWriteCloser
	delay    time.Duration
}

func Delay(rwc io.ReadWriteCloser, delay time.Duration) *Delayer {
	return &Delayer{
		upstream: rwc,
		delay: delay,
	}
}

func (d *Delayer) GetDelay() time.Duration {
	return d.delay
}

func (d *Delayer) Read(p []byte) (int, error) {
	if d.delay != 0 {
		time.Sleep(d.delay)
	}
	return d.upstream.Read(p)
}

func (d *Delayer) Write(p []byte) (int, error) {
	if d.delay != 0 {
		time.Sleep(d.delay)
	}
	return d.upstream.Write(p)
}

func (d *Delayer) Close() error {
	return d.upstream.Close()
}
