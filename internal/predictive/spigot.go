/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2026 Daniel Selifonov
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
	"sync/atomic"
)

// SpigotReader is an io.Reader wrapper that blocks reading until an explict Open operation is invoked. It is safe to
// concurrently invoke Open. Open is idempotent. It is possible to invoke Read concurrently.
type SpigotReader struct {
	reader io.Reader

	block chan struct{}
	open  atomic.Bool
}

func MakeSpigotReader(r io.Reader) *SpigotReader {
	sr := &SpigotReader{
		reader: r,
		block:  make(chan struct{}),
	}
	return sr
}

func (sr *SpigotReader) Open() bool {
	if !sr.open.Swap(true) {
		close(sr.block)
		return true
	}
	return false
}

func (sr *SpigotReader) Read(p []byte) (int, error) {
	if !sr.open.Load() {
		<-sr.block
	}
	return sr.reader.Read(p)
}
