package container

import (
	"testing"
	"time"
)

func TestParseStatReadsAFile(t *testing.T) {
	info, err := parseStat("1234/regular file/644/1700000000\n")

	if err != nil {
		t.Fatalf("parseStat: %v", err)
	}
	if info.Size != 1234 || info.IsDir || info.Mode != 0o644 || !info.ModTime.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("info = %+v", info)
	}
}

func TestParseStatReportsADirectoryWithSizeZero(t *testing.T) {
	info, err := parseStat("4096/directory/755/1700000000\n")

	if err != nil {
		t.Fatalf("parseStat: %v", err)
	}
	if !info.IsDir || info.Size != 0 || info.Mode != 0o755 {
		t.Errorf("info = %+v, want a directory of size 0 and mode 755", info)
	}
}

func TestParseStatKeepsOnlyPermissionAndSpecialBits(t *testing.T) {
	info, err := parseStat("0/regular file/4755/1\n")

	if err != nil {
		t.Fatalf("parseStat: %v", err)
	}
	if info.Mode != 0o4755 {
		t.Errorf("mode = %o, want 4755", info.Mode)
	}
}

func TestParseStatRejectsUnexpectedOutput(t *testing.T) {
	for name, out := range map[string]string{
		"empty":           "",
		"too few fields":  "12/regular file/644",
		"size not number": "x/regular file/644/1",
		"mode not octal":  "1/regular file/9z9/1",
		"mtime not epoch": "1/regular file/644/now",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStat(out); err == nil {
				t.Errorf("parseStat(%q) returned no error", out)
			}
		})
	}
}
