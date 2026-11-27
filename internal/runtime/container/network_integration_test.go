//go:build integration

package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// These tests reach the public internet (pypi.org, example.com): the guard's job
// is to judge real connections, and a fake destination would not exercise the
// firewall, the redirect and the name lookup together.

func networkedSandbox(t *testing.T, allowOut, denyOut []string) (*Driver, runtime.SandboxID) {
	t.Helper()
	d := newAgentDriver(t)
	if err := d.EnableNetwork(); err != nil {
		t.Fatalf("EnableNetwork: %v", err)
	}
	id := runtime.SandboxID("sbx_net_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")))
	_, err := d.Create(context.Background(), runtime.CreateRequest{
		SandboxID: id, TenantID: "tnt_test", Image: pythonImage, AllowOut: allowOut, DenyOut: denyOut,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
	return d, id
}

func python(t *testing.T, d *Driver, id runtime.SandboxID, code string) (exit int, output string) {
	t.Helper()
	res, err := d.Exec(context.Background(), id, runtime.ExecRequest{
		Cmd: []string{"python3", "-c", code}, Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res.ExitCode, string(res.Stdout) + string(res.Stderr)
}

func fetch(url string) string {
	return fmt.Sprintf("import urllib.request\nprint(urllib.request.urlopen(%q, timeout=20).status)", url)
}

const connectTo = `import socket
s = socket.create_connection((%q, %d), timeout=8)
print("connected", s.recv(1))`

func TestNetworkedSandboxReachesThePublicInternetByDefault(t *testing.T) {
	d, id := networkedSandbox(t, nil, nil)

	if exit, out := python(t, d, id, fetch("https://pypi.org/simple/pip/")); exit != 0 || !strings.Contains(out, "200") {
		t.Fatalf("https fetch: exit %d\n%s", exit, out)
	}
	if exit, out := python(t, d, id, fetch("http://example.com/")); exit != 0 || !strings.Contains(out, "200") {
		t.Fatalf("http fetch: exit %d\n%s", exit, out)
	}
}

func TestNetworkedSandboxInstallsAPackageFromPyPI(t *testing.T) {
	d, id := networkedSandbox(t, []string{"pypi.org", "files.pythonhosted.org"}, []string{"0.0.0.0/0"})

	res, err := d.Exec(context.Background(), id, runtime.ExecRequest{
		Cmd:     []string{"python3", "-m", "pip", "install", "--no-cache-dir", "--target", "/workspace/site", "six"},
		Timeout: 120 * time.Second,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("pip install: %v, exit %d\n%s%s", err, res.ExitCode, res.Stdout, res.Stderr)
	}
	if exit, out := python(t, d, id, `import sys; sys.path.insert(0, "/workspace/site"); import six; print(six.__version__)`); exit != 0 {
		t.Fatalf("import the installed package: exit %d\n%s", exit, out)
	}
}

func TestAllowOutLimitsTheSandboxToTheNamedHosts(t *testing.T) {
	d, id := networkedSandbox(t, []string{"pypi.org"}, []string{"0.0.0.0/0"})

	if exit, out := python(t, d, id, fetch("https://pypi.org/simple/pip/")); exit != 0 || !strings.Contains(out, "200") {
		t.Fatalf("the allowed host: exit %d\n%s", exit, out)
	}
	if exit, out := python(t, d, id, fetch("https://example.com/")); exit == 0 {
		t.Errorf("a host outside allow_out answered:\n%s", out)
	}
	if exit, out := python(t, d, id, fetch("http://example.com/")); exit == 0 {
		t.Errorf("a host outside allow_out answered over http:\n%s", out)
	}

	logs, _, err := d.guardLogs(context.Background(), guardName(id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, `"event":"egress_denied"`) || !strings.Contains(logs, `"host":"example.com"`) {
		t.Errorf("the guard did not report the denial:\n%s", logs)
	}
}

func TestNetworkedSandboxCannotReachPrivateAddresses(t *testing.T) {
	d, id := networkedSandbox(t, nil, nil)
	gateway := dockerBridgeGateway(t, d)

	for name, code := range map[string]string{
		"the metadata address":        fmt.Sprintf(connectTo, "169.254.169.254", 80),
		"the host through the bridge": fmt.Sprintf(connectTo, gateway, 22),
		"the egress proxy itself":     fmt.Sprintf(connectTo, "127.0.0.1", 15001),
	} {
		// The proxy accepts the redirected connection and closes it, so the client
		// connects and then reads nothing.
		if exit, out := python(t, d, id, code); exit == 0 && !strings.Contains(out, "b''") {
			t.Errorf("%s answered:\n%s", name, out)
		}
	}

	// Each refusal has to be the guard's decision, not a destination that happens
	// to be unreachable from this machine.
	logs, _, err := d.guardLogs(context.Background(), guardName(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"destination":"169.254.169.254:80"`,
		`"destination":"` + gateway + `:22"`,
		`"destination":"127.0.0.1:15001"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the guard log has no %s:\n%s", want, logs)
		}
	}
}

func TestNetworkedSandboxCannotBypassTheProxy(t *testing.T) {
	d, id := networkedSandbox(t, nil, []string{"0.0.0.0/0"})

	// UDP that is not DNS to the configured resolver is dropped by the firewall.
	udp := `import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.sendto(b"x", ("1.1.1.1", 53))`
	if exit, out := python(t, d, id, udp); exit == 0 || !strings.Contains(out, "PermissionError") {
		t.Errorf("UDP to another resolver was sent:\n%s", out)
	}
	// The mark that exempts the proxy's own sockets needs CAP_NET_ADMIN.
	mark := `import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, 36, 0x4c5342)`
	if exit, out := python(t, d, id, mark); exit == 0 || !strings.Contains(out, "PermissionError") {
		t.Errorf("the sandbox set the proxy's socket mark: exit %d\n%s", exit, out)
	}
	// And with deny-all, nothing is reachable.
	if exit, out := python(t, d, id, fetch("https://pypi.org/simple/pip/")); exit == 0 {
		t.Errorf("deny-all let a fetch through:\n%s", out)
	}
}

func TestDestroyRemovesTheNetworkGuard(t *testing.T) {
	d, id := networkedSandbox(t, nil, nil)
	ctx := context.Background()

	orphans, err := d.ListOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, o := range orphans {
		if o.ID == id {
			seen++
			if o.State != runtime.StateRunning {
				t.Errorf("state = %s, want running", o.State)
			}
		}
	}
	if seen != 1 {
		t.Errorf("the sandbox is listed %d times, want once", seen)
	}

	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, name := range []string{containerName(id), guardName(id)} {
		if _, err := d.cli.ContainerInspect(ctx, name); !errdefs.IsNotFound(err) {
			t.Errorf("%s still exists (inspect: %v)", name, err)
		}
	}
}

func TestInvalidOutboundRuleIsAnInvalidRequest(t *testing.T) {
	d := newAgentDriver(t)
	if err := d.EnableNetwork(); err != nil {
		t.Fatal(err)
	}
	_, err := d.Create(context.Background(), runtime.CreateRequest{
		SandboxID: "sbx_net_invalid_rule", TenantID: "tnt_test", DenyOut: []string{"example.com"},
	})
	if !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("Create = %v, want ErrInvalidRequest", err)
	}
	if _, ierr := d.cli.ContainerInspect(context.Background(), guardName("sbx_net_invalid_rule")); !errdefs.IsNotFound(ierr) {
		t.Errorf("a guard was left behind (inspect: %v)", ierr)
	}
}

func dockerBridgeGateway(t *testing.T, d *Driver) string {
	t.Helper()
	nw, err := d.cli.NetworkInspect(context.Background(), "bridge", network.InspectOptions{})
	if err != nil {
		t.Fatalf("inspect the bridge network: %v", err)
	}
	for _, c := range nw.IPAM.Config {
		if c.Gateway != "" {
			return c.Gateway
		}
	}
	t.Fatal("the bridge network has no gateway")
	return ""
}

func TestUpdateNetworkAppliesToTheNextConnection(t *testing.T) {
	d, id := networkedSandbox(t, nil, nil)
	ctx := context.Background()
	if exit, out := python(t, d, id, fetch("http://example.com/")); exit != 0 {
		t.Fatalf("before the update: exit %d\n%s", exit, out)
	}

	err := d.UpdateNetwork(ctx, id, runtime.NetworkPolicy{AllowOut: []string{"pypi.org"}, DenyOut: []string{"0.0.0.0/0"}})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}

	if exit, out := python(t, d, id, fetch("http://example.com/")); exit == 0 {
		t.Errorf("a host outside the new allow_out answered:\n%s", out)
	}
	if exit, out := python(t, d, id, fetch("https://pypi.org/simple/pip/")); exit != 0 {
		t.Errorf("the host in the new allow_out: exit %d\n%s", exit, out)
	}

	denials, err := d.NetworkDenials(ctx, id, 10)
	if err != nil {
		t.Fatalf("NetworkDenials: %v", err)
	}
	if len(denials) != 1 || denials[0].Host != "example.com" || denials[0].Time.IsZero() || denials[0].Reason == "" {
		t.Errorf("denials = %+v, want one for example.com with a time and a reason", denials)
	}

	// Opening it again works too.
	if err := d.UpdateNetwork(ctx, id, runtime.NetworkPolicy{}); err != nil {
		t.Fatalf("UpdateNetwork to no rules: %v", err)
	}
	if exit, out := python(t, d, id, fetch("http://example.com/")); exit != 0 {
		t.Errorf("after the rules were removed: exit %d\n%s", exit, out)
	}
}

func TestUpdateNetworkRefusesInvalidRulesAndKeepsTheOldOnes(t *testing.T) {
	d, id := networkedSandbox(t, nil, []string{"0.0.0.0/0"})
	ctx := context.Background()

	err := d.UpdateNetwork(ctx, id, runtime.NetworkPolicy{DenyOut: []string{"example.com"}})
	if !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("UpdateNetwork = %v, want ErrInvalidRequest", err)
	}
	if exit, out := python(t, d, id, fetch("http://example.com/")); exit == 0 {
		t.Errorf("the deny-all in force was lost:\n%s", out)
	}
}

func TestNetworkOperationsOnASandboxWithoutOne(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, "")
	ctx := context.Background()

	if err := d.UpdateNetwork(ctx, id, runtime.NetworkPolicy{}); !errors.Is(err, runtime.ErrNoNetwork) {
		t.Errorf("UpdateNetwork = %v, want ErrNoNetwork", err)
	}
	if _, err := d.NetworkDenials(ctx, id, 10); !errors.Is(err, runtime.ErrNoNetwork) {
		t.Errorf("NetworkDenials = %v, want ErrNoNetwork", err)
	}
	if err := d.UpdateNetwork(ctx, "sbx_does_not_exist", runtime.NetworkPolicy{}); !errors.Is(err, runtime.ErrSandboxNotFound) {
		t.Errorf("UpdateNetwork on a missing sandbox = %v, want ErrSandboxNotFound", err)
	}
}
