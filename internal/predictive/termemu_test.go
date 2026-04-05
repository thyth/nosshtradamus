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
	_ "embed"
	"io"
	"slices"
	"testing"
	"time"
)

// A mosh-generated keyframe from a terminal recording of a Linux shell from initial login, running 3 commands with
// the last (a python3 shell) still running and ready to accept input. The dimensions of the terminal window are 100x40.
//
// When this keyframe is restored, subsequent I/O should appear starting from row 27, column 4.
//
//go:embed data/test-keyframe.bin
var testKeyframe []byte

func TestInterposer_CurrentContents(t *testing.T) {
	pipe, _ := io.Pipe()
	pipeReadCloser := io.NopCloser(pipe)
	rwc := CombineReaderWriterCloser(pipeReadCloser, io.Discard, pipeReadCloser)
	interposer := Interpose(rwc, func(interposer *Interposer, epoch uint64, openedAt time.Time) {
		interposer.CloseEpoch(epoch, openedAt)
	}, GetDefaultInterposerOptions())

	if injectErr := interposer.Inject(testKeyframe); injectErr != nil {
		t.Fatal(injectErr)
	}
	postInjectContents := []byte(interposer.CurrentContents(true))
	if !slices.Equal(testKeyframe, postInjectContents) {
		t.Errorf("Unexpected contents: %02x", postInjectContents)
	}
}
