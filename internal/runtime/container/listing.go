package container

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// listScript lists the immediate children of the directory passed as $0, one
// "<size>/<type>/./<name>" line per entry. It needs only sh, find and stat, which
// busybox provides. Exit 44 means the directory does not exist.
const listScript = confineFunc + `confine "$0"
cd -- "$0" 2>/dev/null || exit 44
find . -mindepth 1 -maxdepth 1 -exec stat -c '%s/%F/%n' {} +`

// listLine matches the start of one listScript entry: size, file type, "./name".
var listLine = regexp.MustCompile(`^(\d+)/([a-z ]+)/\./(.*)$`)

// parseListing turns listScript output into file entries. A line that does not
// start a new entry continues the previous name, which is how a file name that
// contains a newline comes out of stat.
func parseListing(out string) []runtime.FileEntry {
	var entries []runtime.FileEntry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		m := listLine.FindStringSubmatch(line)
		if m == nil {
			if len(entries) > 0 {
				entries[len(entries)-1].Name += "\n" + line
			}
			continue
		}
		size, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue // a size that overflows int64 cannot come from stat
		}
		isDir := m[2] == "directory"
		if isDir {
			size = 0 // a directory's own inode size says nothing about its content
		}
		entries = append(entries, runtime.FileEntry{Name: m[3], Size: size, IsDir: isDir})
	}
	return entries
}
