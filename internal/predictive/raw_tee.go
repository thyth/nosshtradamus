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

	"golang.org/x/crypto/ssh"
)

// RawTeeWriter serves as an io.Writer target that transforms payloads into channel requests that don't seek a reply.
// This is intended to support tee-ing the raw/unprocessed data stream of channel output to a specified request stream
// before it is fed into the predictive terminal emulator and other kinds of processing.
type RawTeeWriter struct {
	request string
	sshChan ssh.Channel
	enabled bool
}

func MakeRawTeeWriter(sshChan ssh.Channel, request string) *RawTeeWriter {
	return &RawTeeWriter{
		request: request,
		sshChan: sshChan,
		enabled: true,
	}
}

func (rtw *RawTeeWriter) Write(p []byte) (n int, err error) {
	if rtw.enabled {
		_, err = rtw.sshChan.SendRequest(rtw.request, false, p)
	}
	if err == nil {
		n = len(p)
	}
	return
}

// Enabled allows enabling or disabling the sending of requests by this Tee.
func (rtw *RawTeeWriter) Enabled(enabled bool) {
	rtw.enabled = enabled
}

// ReaderWriterCloserCombiner allows combining separate io.Reader, io.Writer, and io.Closer values into a single
// io.ReadWriteCloser.
type ReaderWriterCloserCombiner struct {
	reader io.Reader
	writer io.Writer
	closer io.Closer
}

func CombineReaderWriterCloser(reader io.Reader, writer io.Writer, closer io.Closer) *ReaderWriterCloserCombiner {
	return &ReaderWriterCloserCombiner{
		reader: reader,
		writer: writer,
		closer: closer,
	}
}

func (combiner *ReaderWriterCloserCombiner) Read(p []byte) (int, error) {
	return combiner.reader.Read(p)
}

func (combiner *ReaderWriterCloserCombiner) Write(p []byte) (int, error) {
	return combiner.writer.Write(p)
}

func (combiner *ReaderWriterCloserCombiner) Close() error {
	return combiner.closer.Close()
}
