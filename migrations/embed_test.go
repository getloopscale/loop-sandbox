package migrations

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestAllEmbeddedMigrationsAreValid(t *testing.T) {
	all, err := All()

	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("All returned no migrations")
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Version >= all[i].Version {
			t.Errorf("migrations out of order: %s before %s", all[i-1].Name, all[i].Name)
		}
	}
}

func TestLoadRejectsAFileNameWithoutAVersion(t *testing.T) {
	fsys := fstest.MapFS{"init.sql": {Data: []byte("SELECT 1")}}

	_, err := load(fsys)

	if err == nil || !strings.Contains(err.Error(), "NNNN_description.sql") {
		t.Errorf("load = %v, want a file-name error", err)
	}
}

func TestLoadRejectsTwoFilesWithTheSameVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("SELECT 1")},
		"0001_b.sql": {Data: []byte("SELECT 2")},
	}

	_, err := load(fsys)

	if err == nil || !strings.Contains(err.Error(), "share version 0001") {
		t.Errorf("load = %v, want a duplicate-version error", err)
	}
}

func TestLoadChecksumChangesWhenTheSQLChanges(t *testing.T) {
	a, errA := load(fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1")}})
	b, errB := load(fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 2")}})

	if errA != nil || errB != nil {
		t.Fatalf("load: %v, %v", errA, errB)
	}
	if a[0].Checksum == b[0].Checksum {
		t.Error("two different SQL bodies produced the same checksum")
	}
}
