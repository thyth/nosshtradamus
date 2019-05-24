package predictive

import (
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh"

	"io"
)

// Mosh (Mobile Shell) implements a terminal emulator capable of predictive/speculative echo and line editing for
// interactive sessions. These predictions are displayed to the user effectively immediately in response to input,
// without waiting for the remote server to echo back output. The server responses are used to confirm and correct these
// predictions, but the immediate local output provides substantially better interactive user experience when these
// server responses are subject to delay.
//
// The Mosh model implements this capability on top of a UDP based state synchronization protocol, and runs terminal
// emulation state tracking on both ends. This allows skipping intermediate updates between the last state synchronized
// to the client and the current state of the server, rather than transmitting a raw terminal octet stream.
//
// The go-mosh wrapper for Mosh exposes the C++ classes implementing terminal emulation, computation of state deltas,
// and prediction. The wrapper does not expose the state synchronization protocol.

func GetVersion() string {
	return mosh.GetVersion()
}

// This package implements a predictive interposer for octet streams representing interactive terminal sessions, which
// leverages the Mosh classes, for injection of responsive UX on the client side (without any requirements on server).
// While not all of Mosh's benefits are available (e.g. instant Ctrl-C), it still provides effectively immediate
// reactivity to user inputs.
//
// This interposer satisfies Go's io.ReadWriteCloser interface. The interposer wraps an upstream io.ReadWriteCloser
// (e.g. a net.Conn, or ssh.Channel). Writes to the interposer are written both to the upstream and to the predictive
// terminal state tracker. Reads from the interposer contain a combination of predictive speculations in response to
// local writes, and state read from the upstream.
type Interposer struct {
	upstream io.ReadWriteCloser
}

func Interpose(rwc io.ReadWriteCloser) *Interposer {
	return &Interposer{
		upstream: rwc,
	}
}

// Close the terminal.
func (i *Interposer) Close() error {
	return i.upstream.Close()
}

// Read printed output from the terminal.
func (i *Interposer) Read(p []byte) (int, error) {
	return i.upstream.Read(p)
}

// Write user input to the terminal.
func (i *Interposer) Write(p []byte) (int, error) {
	return i.upstream.Write(p)
}

// Change the width and height of the interposed terminal, in response to e.g. SIGWINCH or equivalent signal.
func (i *Interposer) Resize(w, h int) {
	// TODO
}

// Produce a "patch" that transforms a fresh/reset terminal to one that matches the current display contents of the
// interposed terminal.
func (i *Interposer) CurrentContents() string {
	return "" // TODO
}
