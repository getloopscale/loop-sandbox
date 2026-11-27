// Package migrations embeds the SQL schema migrations so the binary is
// self-contained. Files are named NNNN_description.sql and applied in version order
// by postgres.Migrate, which records each applied version.
package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
)

//go:embed *.sql
var files embed.FS

// Migration is one schema change.
type Migration struct {
	Version  string // the numeric prefix of the file name, e.g. "0007"
	Name     string // the file name
	SQL      string
	Checksum string // hex SHA-256 of SQL, to detect an applied migration that was edited
}

var fileName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// All returns the embedded migrations in apply order. It fails when a file name
// does not follow NNNN_description.sql or two files share a version, because either
// would make the apply order ambiguous.
func All() ([]Migration, error) {
	return load(files)
}

func load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	out := make([]Migration, 0, len(names))
	seen := map[string]string{}
	for _, name := range names {
		m := fileName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %q: name must match NNNN_description.sql", name)
		}
		if other, dup := seen[m[1]]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %s", other, name, m[1])
		}
		seen[m[1]] = name
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{Version: m[1], Name: name, SQL: string(raw), Checksum: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

// Ordered returns the migration SQL statements in apply order, for tests that build
// a schema on a throwaway database. A real database goes through postgres.Migrate,
// which records the applied versions.
func Ordered() []string {
	all, err := All()
	if err != nil {
		panic(err) // the embedded set is checked by TestAllEmbeddedMigrationsAreValid
	}
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.SQL)
	}
	return out
}
