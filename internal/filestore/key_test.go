package filestore_test

import (
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/filestore"
)

func TestTenantKey_namespaces_by_tenant(t *testing.T) {
	a := filestore.TenantKey("tenant-a", "sbx-1", "out.bin")
	b := filestore.TenantKey("tenant-b", "sbx-1", "out.bin")
	if a == b {
		t.Fatal("different tenants produced the same key")
	}
	if !strings.HasPrefix(a, "tenants/tenant-a/") || !strings.HasPrefix(b, "tenants/tenant-b/") {
		t.Errorf("keys not tenant-prefixed: %q %q", a, b)
	}
}
