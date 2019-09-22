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
