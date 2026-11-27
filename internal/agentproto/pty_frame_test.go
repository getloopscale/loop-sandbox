package agentproto

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestPTYDataFrameRoundTrips(t *testing.T) {
	var wire bytes.Buffer

	if err := WritePTYData(&wire, []byte("ls -la\n")); err != nil {
		t.Fatalf("WritePTYData: %v", err)
	}
	kind, payload, err := ReadPTYFrame(&wire)

	if err != nil || kind != PTYFrameData || string(payload) != "ls -la\n" {
		t.Errorf("ReadPTYFrame = (%d, %q, %v), want data frame %q", kind, payload, err, "ls -la\n")
	}
}

func TestPTYResizeFrameRoundTrips(t *testing.T) {
	var wire bytes.Buffer

	if err := WritePTYResize(&wire, 40, 100); err != nil {
		t.Fatalf("WritePTYResize: %v", err)
	}
	kind, payload, err := ReadPTYFrame(&wire)
	if err != nil || kind != PTYFrameResize {
		t.Fatalf("ReadPTYFrame = (%d, _, %v), want a resize frame", kind, err)
	}
	rows, cols, err := DecodePTYResize(payload)

	if err != nil || rows != 40 || cols != 100 {
		t.Errorf("DecodePTYResize = (%d, %d, %v), want (40, 100, nil)", rows, cols, err)
	}
}

func TestPTYDataLargerThanOneFrameIsSplitAndReassembles(t *testing.T) {
	input := bytes.Repeat([]byte("x"), 2*MaxPTYFramePayload+17)
	var wire bytes.Buffer

	if err := WritePTYData(&wire, input); err != nil {
		t.Fatalf("WritePTYData: %v", err)
	}

	var got []byte
	frames := 0
	for {
		kind, payload, err := ReadPTYFrame(&wire)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || kind != PTYFrameData {
			t.Fatalf("frame %d: kind %d, err %v", frames, kind, err)
		}
		got = append(got, payload...)
		frames++
	}
	if frames != 3 || !bytes.Equal(got, input) {
		t.Errorf("got %d frames carrying %d bytes, want 3 frames carrying %d", frames, len(got), len(input))
	}
}

func TestReadPTYFrameReportsAConnectionCutInsideAFrame(t *testing.T) {
	var wire bytes.Buffer
	if err := WritePTYData(&wire, []byte("abcdef")); err != nil {
		t.Fatalf("WritePTYData: %v", err)
	}
	cut := bytes.NewReader(wire.Bytes()[:wire.Len()-2])

	_, _, err := ReadPTYFrame(cut)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadPTYFrame = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadPTYFrameRejectsAPayloadOverTheLimit(t *testing.T) {
	header := []byte{byte(PTYFrameData), 0xFF, 0xFF}

	_, _, err := ReadPTYFrame(bytes.NewReader(header))

	if err == nil {
		t.Error("ReadPTYFrame accepted a 65535 byte frame, over the limit")
	}
}

func TestDecodePTYResizeRejectsAPayloadOfTheWrongSize(t *testing.T) {
	if _, _, err := DecodePTYResize([]byte{0, 1, 2}); err == nil {
		t.Error("DecodePTYResize accepted a 3 byte payload")
	}
}
