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

import "io"

// Enable an alternative io.ReadWriteCloser implementation, otherwise just pass through.

type IoSwitch struct {
	passthrough io.ReadWriteCloser
	refractor   io.ReadWriteCloser
	enabled     bool
}

func MakeIoSwitch(passthrough io.ReadWriteCloser) *IoSwitch {
	return &IoSwitch{
		passthrough: passthrough,
		refractor:   nil,
		enabled:     false,
	}
}

func (ios *IoSwitch) Read(p []byte) (int, error) {
	if ios.enabled {
		return ios.refractor.Read(p)
	} else {
		return ios.passthrough.Read(p)
	}
}

func (ios *IoSwitch) Write(p []byte) (int, error) {
	if ios.enabled {
		return ios.refractor.Write(p)
	} else {
		return ios.passthrough.Write(p)
	}
}

func (ios *IoSwitch) Close() error {
	if ios.enabled {
		return ios.refractor.Close()
	} else {
		return ios.passthrough.Close()
	}
}

func (ios *IoSwitch) Enable(refractor io.ReadWriteCloser) {
	if !ios.enabled {
		ios.refractor = refractor
		ios.enabled = true
	}
}
