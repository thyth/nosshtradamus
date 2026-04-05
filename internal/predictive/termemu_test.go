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
	"bytes"
	_ "embed"
	"encoding/base64"
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

func TestInterposerInjectCurrentContentsRoundtrip(t *testing.T) {
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

func TestInterposerInitializeFollowedByData(t *testing.T) {
	pipe, pipeWriter := io.Pipe()
	pipeReadCloser := io.NopCloser(pipe)
	outputBuffer := &bytes.Buffer{}
	rwc := CombineReaderWriterCloser(pipeReadCloser, io.Discard, pipeReadCloser)
	options := GetDefaultInterposerOptions()
	options.SkipInitialize = true
	options.SkipOpen = true
	interposer := Interpose(rwc, func(interposer *Interposer, epoch uint64, openedAt time.Time) {
		interposer.CloseEpoch(epoch, openedAt)
	}, options)
	//goland:noinspection GoUnhandledErrorResult
	go io.Copy(outputBuffer, interposer)

	if injectErr := interposer.Inject(testKeyframe); injectErr != nil {
		t.Fatal(injectErr)
	}
	outputBuffer.Reset()
	if _, writeErr := pipeWriter.Write([]byte("a")); writeErr != nil {
		t.Fatal(writeErr)
	}
	evalKeyframe := []byte(interposer.CurrentContents(true))
	expectedContents, _ := base64.StdEncoding.DecodeString(
		"G1s4OzQwOzEwMHQbXTA7dWJ1bnR1QGNocm9uaXRvbjogfgcbWz81bBtbchtbMG0bW0gbWzJKG1s/MjVsV2VsY29tZSB0byBVYnVudHUg" +
			"MjIuMDQuNSBMVFMgKEdOVS9MaW51eCA0LjQuMCB4ODZfNjQpG1tLDQobW0sKICogRG9jdW1lbnRhdGlvbjogIGh0dHBzOi8vaGVscC51" +
			"YnVudHUuY29tG1tLDQogKiBNYW5hZ2VtZW50OiAgICAgaHR0cHM6Ly9sYW5kc2NhcGUuY2Fub25pY2FsLmNvbRtbSw0KICogU3VwcG9y" +
			"dDogICAgICAgIGh0dHBzOi8vdWJ1bnR1LmNvbS9wcm8bW0sNClRvIHJ1biBhIGNvbW1hbmQgYXMgYWRtaW5pc3RyYXRvciAodXNlciAi" +
			"cm9vdCIpLCB1c2UgInN1ZG8gPGNvbW1hbmQ+Ii4bW0sNClNlZSAibWFuIHN1ZG9fcm9vdCIgZm9yIGRldGFpbHMuG1tLDQobW0sKG1sw" +
			"OzE7MzJtdWJ1bnR1QGNocm9uaXRvbhtbMG06G1swOzE7MzRtfhtbMG0kIHBzIGF1eBtbSw0KVVNFUiAgICAgICBQSUQgJUNQVSAlTUVN" +
			"ICAgIFZTWiAgIFJTUyBUVFkgICAgICBTVEFUIFNUQVJUICAgVElNRSBDT01NQU5EG1tLDQpyb290ICAgICAgICAgMSAgMi44ICAwLjAg" +
			"IDEwODQ4ICAzODc2ID8gICAgICAgIFNzICAgMDY6NTYgICAwOjAwIC9zYmluL3RpbmkgLS0gL3NiaW4vY2hyb25pdG9uZBtbSw0Kcm9v" +
			"dCAgICAgICAgIDIgMTQuNCAgMC40IDEyMzgyMDQgMTYyNTIgPyAgICAgICBTbCAgIDA2OjU2ICAgMDowMiAvc2Jpbi9jaHJvbml0b25k" +
			"G1tLDQpyb290ICAgICAgICAgOSAxMy4zICAwLjEgIDE1Mzk2ICA3MjQ4ID8gICAgICAgIFNzICAgMDY6NTcgICAwOjAxIC9iaW4vbG9n" +
			"aW4gLXAgLWYbW0sNCnVidW50dSAgICAgIDE5IDEyLjggIDAuMiAgMTgzMDQgMTA0MzIgPyAgICAgICAgUyAgICAwNjo1NyAgIDA6MDAg" +
			"LWJhc2gbW0sNCnVidW50dSAgICAgIDI4IDU4LjAgIDAuMyAgMjA3NDAgMTI4NzYgPyAgICAgICAgUiAgICAwNjo1NyAgIDA6MDAgcHMg" +
			"YXV4G1tLDQobWzA7MTszMm11YnVudHVAY2hyb25pdG9uG1swbTobWzA7MTszNG1+G1swbSQgZGYgLVQgLWgbW0sNCkZpbGVzeXN0ZW0g" +
			"ICAgIFR5cGUgICBTaXplICBVc2VkIEF2YWlsIFVzZSUgTW91bnRlZCBvbhtbSw0Kbm9uZSAgICAgICAgICAgOXAgICAgMTAwOU0gIDYz" +
			"NE0gIDM3NU0gIDYzJSAvG1tLDQpub25lICAgICAgICAgICBkZXYgICAgMi4wRyAgICAgMCAgMi4wRyAgIDAlIC9kZXYbW0sNCm5vbmUg" +
			"ICAgICAgICAgIHRtcGZzICAyLjBHICAgICAwICAyLjBHICAgMCUgL3RtcBtbSw0KG1swOzE7MzJtdWJ1bnR1QGNocm9uaXRvbhtbMG06" +
			"G1swOzE7MzRtfhtbMG0kIHB5dGhvbjMbW0sNClB5dGhvbiAzLjEwLjEyIChtYWluLCBBdWcgMTUgMjAyNSwgMTQ6MzI6NDMpIFtHQ0Mg" +
			"MTEuNC4wXSBvbiBsaW51eBtbSw0KVHlwZSAiaGVscCIsICJjb3B5cmlnaHQiLCAiY3JlZGl0cyIgb3IgImxpY2Vuc2UiIGZvciBtb3Jl" +
			"IGluZm9ybWF0aW9uLhtbSw0KPj4+IGEgPSAxMjMbW0sNCj4+PiBhG1tLDQoxMjMbW0sNCj4+PiBhG1tLDQobW0sKG1tLChtbSwobW0sK" +
			"G1tLChtbSwobW0sKG1tLChtbSwobW0sKG1tLChtbSwobW0sbWzI3OzZIG1s/MjVoG1swbRtbPzIwMDRsG1s/MTAwM2wbWz8xMDAybBtb" +
			"PzEwMDFsG1s/MTAwMGwbWz8xMDA0bBtbPzEwMTVsG1s/MTAwNmwbWz8xMDA1bA==")
	if !slices.Equal(expectedContents, evalKeyframe) {
		t.Errorf("Unexpected keyframe: %02x", evalKeyframe)
	}
	time.Sleep(100 * time.Millisecond)
	if outBytes := outputBuffer.Bytes(); !slices.Equal([]byte("a"), outBytes) {
		t.Errorf("Unexpected output buffer contents: %02x", outBytes)
	}

	outputBuffer.Reset()
	if _, writeErr := pipeWriter.Write([]byte("b")); writeErr != nil {
		t.Fatal(writeErr)
	}
	time.Sleep(100 * time.Millisecond)
	if outBytes := outputBuffer.Bytes(); !slices.Equal([]byte("b"), outBytes) {
		t.Errorf("Unexpected output buffer contents: %02x", outBytes)
	}
}
