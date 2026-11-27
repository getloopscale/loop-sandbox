package agentproto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

func TestRoundTrip_request_and_response(t *testing.T) {
	var buf bytes.Buffer
	req := &agentproto.Request{
		Op:      agentproto.OpExec,
		Cmd:     []string{"echo", "hi"},
		Env:     map[string]string{"A": "1"},
		WorkDir: "/workspace",
		Data:    []byte{0x00, 0xff, 0x10}, // binary-safe
	}
	if err := agentproto.WriteFrame(&buf, req); err != nil {
		t.Fatalf("WriteFrame req: %v", err)
	}
	got, err := agentproto.ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if got.Op != agentproto.OpExec || len(got.Cmd) != 2 || got.Cmd[1] != "hi" ||
		got.Env["A"] != "1" || got.WorkDir != "/workspace" || !bytes.Equal(got.Data, req.Data) {
		t.Errorf("request round-trip mismatch: %+v", got)
	}

	resp := &agentproto.Response{ExitCode: 3, Stdout: []byte("out"), Stderr: []byte("err")}
	if err := agentproto.WriteFrame(&buf, resp); err != nil {
		t.Fatalf("WriteFrame resp: %v", err)
	}
	gotR, err := agentproto.ReadResponse(&buf)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if gotR.ExitCode != 3 || string(gotR.Stdout) != "out" || string(gotR.Stderr) != "err" {
		t.Errorf("response round-trip mismatch: %+v", gotR)
	}
}

// A declared length over the cap must fail-fast, not allocate unbounded memory.
func TestReadFrame_rejects_oversize_length(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], (32<<20)+1)
	_, err := agentproto.ReadRequest(bytes.NewReader(hdr[:]))
	if !errors.Is(err, agentproto.ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadRequest_truncated_frame_errors(t *testing.T) {
	// header says 10 bytes but only 3 follow → io.ErrUnexpectedEOF path.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 10)
	buf := append(hdr[:], []byte("abc")...)
	if _, err := agentproto.ReadRequest(bytes.NewReader(buf)); err == nil {
		t.Error("expected error on truncated frame, got nil")
	}
}

func TestReadResponse_malformed_json_errors(t *testing.T) {
	var buf bytes.Buffer
	// write a valid frame whose body is not valid JSON
	body := []byte("{not json")
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	buf.Write(hdr[:])
	buf.Write(body)
	if _, err := agentproto.ReadResponse(&buf); err == nil {
		t.Error("expected decode error on malformed JSON, got nil")
	}
}

func TestWriteFrame_rejects_oversize_body(t *testing.T) {
	// A response whose base64-encoded body exceeds the 32 MiB frame cap must be
	// rejected on the WRITE side too (not only on read).
	var buf bytes.Buffer
	resp := &agentproto.Response{Data: make([]byte, 25<<20)} // base64 ≈ 33 MiB > cap
	if err := agentproto.WriteFrame(&buf, resp); !errors.Is(err, agentproto.ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestRoundTrip_stdin_and_path_fields(t *testing.T) {
	var buf bytes.Buffer
	req := &agentproto.Request{Op: agentproto.OpWrite, Path: "/workspace/x", StdinB64: []byte("piped")}
	if err := agentproto.WriteFrame(&buf, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := agentproto.ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if got.Path != "/workspace/x" || string(got.StdinB64) != "piped" {
		t.Errorf("round-trip Path=%q Stdin=%q, want '/workspace/x' 'piped'", got.Path, got.StdinB64)
	}
}
