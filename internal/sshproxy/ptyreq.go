package sshproxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type PtyReqData struct {
	Term   string
	Width  uint32
	Height uint32
}

func InterpretPtyReq(payload []byte) (*PtyReqData, error) {
	r := bytes.NewReader(payload)
	termLen := uint32(0)
	width := uint32(0)
	height := uint32(0)
	if e := binary.Read(r, binary.BigEndian, &termLen); e != nil {
		return nil, e
	}
	term := make([]byte, termLen)
	if e := binary.Read(r, binary.BigEndian, &term); e != nil {
		return nil, e
	}
	if e := binary.Read(r, binary.BigEndian, &width); e != nil {
		return nil, e
	}
	if e := binary.Read(r, binary.BigEndian, &height); e != nil {
		return nil, e
	}
	return &PtyReqData{
		Term:   string(term),
		Width:  width,
		Height: height,
	}, nil
}

func (prd *PtyReqData) String() string {
	if prd == nil {
		return "<nil>"
	} else {
		return fmt.Sprintf("[Term: %s, Width: %d, Height: %d]", prd.Term, prd.Width, prd.Height)
	}
}

type WindowChange struct {
	Width  uint32
	Height uint32
}

func InterpretWindowChange(payload []byte) (*WindowChange, error) {
	r := bytes.NewReader(payload)
	width := uint32(0)
	height := uint32(0)
	if e := binary.Read(r, binary.BigEndian, &width); e != nil {
		return nil, e
	}
	if e := binary.Read(r, binary.BigEndian, &height); e != nil {
		return nil, e
	}
	return &WindowChange{
		Width:  width,
		Height: height,
	}, nil
}

func (wc *WindowChange) Serialize() []byte {
	buf := &bytes.Buffer{}
	if e := binary.Write(buf, binary.BigEndian, wc.Width); e != nil {
		return nil
	}
	if e := binary.Write(buf, binary.BigEndian, wc.Height); e != nil {
		return nil
	}
	// window changes have two additional pixel-unit width/height values that we basically ignore...
	if e := binary.Write(buf, binary.BigEndian, wc.Width*8); e != nil {
		return nil
	}
	if e := binary.Write(buf, binary.BigEndian, wc.Height*8); e != nil {
		return nil
	}

	return buf.Bytes()
}

func (wc *WindowChange) String() string {
	return fmt.Sprintf("WindowChange{Width: %d, Height: %d}", wc.Width, wc.Height)
}
