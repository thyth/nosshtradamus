/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2025 Daniel Selifonov
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

	ttyrec3util "gitlab.hive.thyth.com/chronostruct/ttyrec3/pkg/util"
)

// StdoutFilter is an io.ReadWriteCloser reader filter that processes standard out intended for the mosh emulator.
//
// The mosh terminal emulator apparently has intentionally chosen not to implement ISO 2022 "locking escapes", which are
// still used by some applications for graphical character drawing in terminal output, requiring emission of UTF-8
// glyphs for these purposes.
//
// This has generated user confusion, even in mosh upstream: https://github.com/mobile-shell/mosh/issues/1155.
// And also confusion for me: https://gitlab.hive.thyth.com/chronostruct/go-mosh/-/issues/1
//
// Only after reaching the bottom of the rabbit hole investigating this behavior were the remarks on the mosh main page
// understandable as intentional behavior: https://mosh.org/#:~:text=ISO%202022%20locking%20escapes. Further, following
// downstream links in the section revealed: https://www.pps.jussieu.fr/~jch/software/luit/ (or currently maintained via
// https://invisible-island.net/luit/), which is a terminal emulating converter to transform these ISO 2022 escapes into
// UTF-8 glyphs.
//
// Unfortunately, I had already written my own terminal emulation layer and conversion utility ahead of discovering that
// this is already a resolved problem, with that specific name. I also disagree with the decision to skip implementing
// support for stateful switching into graphical special character codeset modes in mosh, as even today there are
// circumstances where terminals will encounter such escape sequences.
//
// This utility uses an internal implementation of a luit-like conversion of graphical codeset ASCII commands into their
// UTF-8 equivalent glyphs for standard out data, prior to feeding it into the mosh terminal emulator.
//
// It is default enabled for nosshtradamus interactive shells, but can be disabled (prior to 'pty-req') per-channel.
type StdoutFilter struct {
	upstream io.ReadWriteCloser

	xtermEmulator ttyrec3util.SeqProcessor[byte, byte]
	pullSlice     *ttyrec3util.PullSliceCloned[byte]

	emitter ttyrec3util.ResumableEmitter[byte]
}

// MakeStdoutFilter around an upstream io.ReadWriteCloser. Options are optional; supply 0 or 1 only.
func MakeStdoutFilter(upstream io.ReadWriteCloser, opts ...*ttyrec3util.XtermProcessorOptions) *StdoutFilter {
	opt := &ttyrec3util.XtermProcessorOptions{
		DisableGraphicalCodesetFilter: false, // this is the default, but spelling out the value/intent explicitly here
	}
	if len(opts) > 0 {
		opt = opts[0]
	}
	return &StdoutFilter{
		upstream:      upstream,
		xtermEmulator: ttyrec3util.XtermProcessor(opt),
		pullSlice:     ttyrec3util.MakePullSliceCloned(make([]byte, 0)),
	}
}

// Read interposes on terminal output and applies modifications imposed by ttyrec3util.XtermProcessor.
func (sf *StdoutFilter) Read(p []byte) (n int, err error) {
	if sf.emitter == nil {
		upN, upErr := sf.upstream.Read(p)
		if upErr != nil {
			return 0, upErr
		}
		sf.pullSlice.Replace(p[:upN])
		sf.emitter = sf.xtermEmulator.Push(sf.pullSlice.PullSeq())
	}

	l := len(p)
	seq2, state := ttyrec3util.Seq2ForEmitter(sf.emitter)
	for elem := range seq2 {
		p[n] = elem
		n++
		if n >= l {
			break
		}
	}
	if *state == ttyrec3util.EmitterAwaitingInput {
		sf.emitter = nil
	}
	return
}

// Write on StdoutFilter passes through to the upstream io.ReadWriteCloser unmodified.
func (sf *StdoutFilter) Write(p []byte) (int, error) {
	return sf.upstream.Write(p)
}

// Close on StdoutFilter passes through to the upstream io.ReadWriteCloser unmodified.
func (sf *StdoutFilter) Close() error {
	return sf.upstream.Close()
}
