package agentproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// The interactive terminal channel is raw bytes from the guest to the host, and
// framed from the host to the guest, so a window resize can travel on the same
// connection as the keystrokes without being mistaken for input.
//
// A frame is one kind byte, a big-endian uint16 payload length and the payload.

// PTYFrameKind identifies a host-to-guest terminal frame.
type PTYFrameKind byte

const (
	// PTYFrameData carries terminal input bytes.
	PTYFrameData PTYFrameKind = 0
	// PTYFrameResize carries the new window size: rows then cols, big-endian uint16 each.
	PTYFrameResize PTYFrameKind = 1
)

// MaxPTYFramePayload is the largest payload of one frame.
const MaxPTYFramePayload = 32 << 10

const ptyFrameHeader = 3

// PTYGuestPort is the guest vsock port that serves the framed terminal channel.
// It differs from the port of the earlier unframed channel (1025) on purpose: a
// host paired with an older guest image fails to connect instead of typing frame
// headers into the shell.
const PTYGuestPort = 1026

// WritePTYData writes p as one or more data frames. Each frame is a single Write,
// so frames from concurrent callers that serialize their calls never interleave.
func WritePTYData(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), MaxPTYFramePayload)
		if err := writePTYFrame(w, PTYFrameData, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// WritePTYResize writes one resize frame.
func WritePTYResize(w io.Writer, rows, cols uint16) error {
	var size [4]byte
	binary.BigEndian.PutUint16(size[0:2], rows)
	binary.BigEndian.PutUint16(size[2:4], cols)
	return writePTYFrame(w, PTYFrameResize, size[:])
}

func writePTYFrame(w io.Writer, kind PTYFrameKind, payload []byte) error {
	frame := make([]byte, ptyFrameHeader+len(payload))
	frame[0] = byte(kind)
	binary.BigEndian.PutUint16(frame[1:3], uint16(len(payload))) //nolint:gosec // len <= MaxPTYFramePayload < 65536
	copy(frame[ptyFrameHeader:], payload)
	_, err := w.Write(frame)
	return err
}

// ReadPTYFrame reads one frame. It returns io.EOF only at a frame boundary; a
// connection that ends inside a frame is io.ErrUnexpectedEOF.
func ReadPTYFrame(r io.Reader) (PTYFrameKind, []byte, error) {
	var header [ptyFrameHeader]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	kind := PTYFrameKind(header[0])
	size := int(binary.BigEndian.Uint16(header[1:3]))
	if size > MaxPTYFramePayload {
		return 0, nil, fmt.Errorf("pty frame of %d bytes exceeds the %d byte limit", size, MaxPTYFramePayload)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return kind, payload, nil
}

// DecodePTYResize extracts rows and cols from a resize frame payload.
func DecodePTYResize(payload []byte) (rows, cols uint16, err error) {
	if len(payload) != 4 {
		return 0, 0, fmt.Errorf("pty resize payload is %d bytes, want 4", len(payload))
	}
	return binary.BigEndian.Uint16(payload[0:2]), binary.BigEndian.Uint16(payload[2:4]), nil
}
