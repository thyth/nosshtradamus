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
	"errors"

	ttyrec3util "gitlab.hive.thyth.com/chronostruct/ttyrec3/pkg/util"
)

// PayloadWindowDimensions extracts the first window dimension command sequence width and height in the payload data. If
// no window dimension command is found in the data, an error will be returned.
func PayloadWindowDimensions(data []byte) (width, height int, err error) {
	found := false
	xtermEmu := ttyrec3util.XtermProcessor(&ttyrec3util.XtermProcessorOptions{
		DisableGraphicalCodesetFilter: true,
		OnResize: func(capturedWidth int, capturedHeight int) {
			width = capturedWidth
			height = capturedHeight
			found = true
		},
	})

	emitter := xtermEmu.Push(ttyrec3util.PullSlice(data))
	seq2, state := ttyrec3util.Seq2ForEmitter(emitter)
	for *state != ttyrec3util.EmitterAwaitingInput {
		for range seq2 {
			if found {
				return
			}
		}
	}

	if !found {
		err = errors.New("could not find window dimension sequence in payload data")
	}
	return
}
