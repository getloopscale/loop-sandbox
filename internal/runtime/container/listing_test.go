package container

import (
	"reflect"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestParseListingReturnsNameSizeAndDirectoryFlag(t *testing.T) {
	out := "5/regular file/./d.txt\n4096/directory/./src\n0/regular empty file/./empty\n"

	got := parseListing(out)

	want := []runtime.FileEntry{
		{Name: "d.txt", Size: 5},
		{Name: "src", IsDir: true},
		{Name: "empty", Size: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListing = %+v, want %+v", got, want)
	}
}

func TestParseListingOfAnEmptyDirectoryIsEmpty(t *testing.T) {
	if got := parseListing(""); len(got) != 0 {
		t.Errorf("parseListing(\"\") = %+v, want no entries", got)
	}
}

func TestParseListingKeepsANewlineInsideAFileName(t *testing.T) {
	out := "3/regular file/./first\nsecond\n7/regular file/./other\n"

	got := parseListing(out)

	want := []runtime.FileEntry{{Name: "first\nsecond", Size: 3}, {Name: "other", Size: 7}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListing = %+v, want %+v", got, want)
	}
}

func TestParseListingKeepsSlashLikeTextInsideAFileName(t *testing.T) {
	out := "2/regular file/./9/directory/.\n"

	got := parseListing(out)

	want := []runtime.FileEntry{{Name: "9/directory/.", Size: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListing = %+v, want %+v", got, want)
	}
}
