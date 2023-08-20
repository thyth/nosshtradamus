/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2019-2023 Daniel Selifonov
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
	"sync"
	"time"
)

type RingDelayer struct {
	upstream io.ReadWriteCloser
	delay    time.Duration

	ring     [][]byte
	sendTime []time.Time
	head     int
	tail     int

	cond *sync.Cond

	termination error
	notifyChan  chan interface{}
}

func RingDelay(rwc io.ReadWriteCloser, delay time.Duration, ringSize int) *RingDelayer {
	rd := &RingDelayer{
		upstream: rwc,
		delay:    delay,

		ring:     make([][]byte, ringSize),
		sendTime: make([]time.Time, ringSize),
		head:     0,
		tail:     0,

		cond: sync.NewCond(&sync.Mutex{}),

		termination: nil,
		notifyChan:  make(chan interface{}, ringSize),
	}
	go func(rd *RingDelayer) {
		for range rd.notifyChan {
			rd.cond.L.Lock()

			now := time.Now()
			headTime := rd.sendTime[rd.head]
			wait := headTime.Sub(now)
			buffer := rd.ring[rd.head]

			if wait > 0 {
				// have time to wait -- release mutex and sleep
				rd.cond.L.Unlock()
				time.Sleep(wait)
				rd.cond.L.Lock()
			}

			rd.ring[rd.head] = nil
			rd.head++
			rd.head %= len(rd.ring)
			rd.cond.Signal() // notify one waiting client (if any) that there is now room in the ring
			rd.cond.L.Unlock()

			_, err := rd.upstream.Write(buffer)
			rd.cond.L.Lock()
			if err != nil {
				rd.termination = err
				close(rd.notifyChan)
			}
			rd.cond.L.Unlock()
		}
	}(rd)
	return rd
}

func (rd *RingDelayer) Close() error {
	if rd.termination != nil {
		return rd.termination
	}
	rd.termination = io.EOF
	close(rd.notifyChan)
	return rd.upstream.Close()
}

func (rd *RingDelayer) Read(p []byte) (int, error) {
	// read is instant -- only writes are delayed for ring delay
	return rd.upstream.Read(p)
}

func (rd *RingDelayer) Write(p []byte) (int, error) {
	if rd.termination != nil {
		return 0, rd.termination
	}
	now := time.Now()
	sendTime := now.Add(rd.delay)
	buffer := make([]byte, len(p))
	copy(buffer, p)

	rd.cond.L.Lock()
	for rd.ring[rd.tail] != nil {
		// wrapped around the ring; wait until there is space available (possible longer delay)
		rd.cond.Wait()
	}

	rd.ring[rd.tail] = buffer
	rd.sendTime[rd.tail] = sendTime
	rd.tail++
	rd.tail %= len(rd.ring)

	rd.cond.L.Unlock()
	if rd.termination != nil {
		return 0, rd.termination
	}
	rd.notifyChan <- true
	return len(p), rd.termination
}

func (rd *RingDelayer) Callback(cb func()) {
	// for simulation/testing of associated events on the same timescale
	time.AfterFunc(rd.delay, cb)
}
