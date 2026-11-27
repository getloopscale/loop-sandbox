//go:build conformance

package conformance

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Known ways out of a sandbox, tried from inside one. Every case asserts that
// the attempt fails, so a case that fails here is a way out.
//
// The first block holds for every driver. The second holds for a driver that
// shares the host kernel with the sandbox, where the kernel's own controls are
// the boundary; inside a microVM the same commands act on a guest kernel and
// say nothing about the host.
func TestEscape(t *testing.T) {
	supported["escape"] = true
	// in runs a shell script in a fresh sandbox and returns its trimmed output.
	in := func(t *testing.T, script string) string {
		t.Helper()
		return strings.TrimSpace(sh(t, newSandbox(t, shellImage).ID, script).Stdout)
	}
	cases := map[string]func(t *testing.T){
		"no_container_runtime_socket_is_reachable": func(t *testing.T) {
			got := in(t, "for s in /var/run/docker.sock /run/docker.sock /run/containerd/containerd.sock /run/podman/podman.sock "+
				"/var/run/crio/crio.sock /run/firecracker.socket; do [ -e $s ] && echo $s; done; "+
				"grep -c -E 'docker\\.sock|containerd\\.sock' /proc/mounts")

			if got != "0" {
				t.Errorf("a runtime socket is visible in the sandbox: %q", got)
			}
		},
		"the_cloud_metadata_service_is_unreachable": func(t *testing.T) {
			got := in(t, "for url in http://169.254.169.254/latest/meta-data/ http://169.254.170.2/ http://metadata.google.internal/; do "+
				"wget -q -T 3 -O - $url >/dev/null 2>&1 && echo reached:$url; done; echo done")

			if got != "done" {
				t.Errorf("the metadata service answered from inside the sandbox: %q", got)
			}
		},
		"the_processes_of_the_host_are_not_visible": func(t *testing.T) {
			// The sandbox sees its own init, the shell of this command and little
			// else. A host /proc would show the container runtime or the API. The
			// bracket in each pattern keeps grep from matching this command line.
			got := in(t, "n=0; for d in /proc/[0-9]*; do n=$((n+1)); done; echo $n; "+
				"cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\0' ' ' | grep -c -E 'docker[d]|container[d]|firecracke[r] --|postgre[s]'")
			count, hostHits, _ := strings.Cut(got, "\n")

			if strings.TrimSpace(hostHits) != "0" {
				t.Errorf("host processes are visible: %q", got)
			}
			if len(count) > 2 { // 100 or more processes
				t.Errorf("the sandbox sees %s processes, more than it started", count)
			}
		},
		"one_sandbox_cannot_read_the_files_of_another": func(t *testing.T) {
			victim, attacker := newSandbox(t, shellImage), newSandbox(t, shellImage)
			writeFile(t, victim.ID, "/workspace/secret-of-the-victim.txt", "s3cret-marker")

			res := sh(t, attacker.ID, "find / -name 'secret-of-the-victim.txt' 2>/dev/null; "+
				"grep -r -l s3cret-marker /workspace /tmp /proc/*/root/workspace 2>/dev/null; echo done")

			if strings.TrimSpace(res.Stdout) != "done" {
				t.Errorf("the other sandbox found the file: %q", res.Stdout)
			}
		},
		"a_file_path_cannot_leave_the_workspace_through_the_api": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			for _, path := range []string{"/etc/passwd", "/workspace/../etc/passwd", "/proc/self/environ", "/workspace/%2e%2e/etc/passwd"} {
				resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files"+path, nil)
				if resp.status == http.StatusOK {
					t.Errorf("GET files%s answered 200 with %d bytes", path, len(resp.body))
				}
			}
		},
		"a_symlink_cannot_lead_the_file_api_out_of_the_workspace": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "ln -s /etc /workspace/out; ln -s /etc/passwd /workspace/passwd-link")

			for _, path := range []string{"/workspace/out/passwd", "/workspace/passwd-link"} {
				resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files"+path, nil)
				if resp.status == http.StatusOK && strings.Contains(string(resp.body), "root:") {
					t.Errorf("GET files%s returned a file from outside the workspace", path)
				}
			}
		},
	}
	if driverName == "container" {
		sharedKernel := map[string]func(t *testing.T){
			"commands_do_not_run_as_root": func(t *testing.T) {
				if got := in(t, "id -u"); got == "0" || got == "" {
					t.Errorf("uid = %q, want a non-root user", got)
				}
			},
			"the_process_holds_no_capability_and_cannot_gain_one": func(t *testing.T) {
				got := in(t, "grep -E '^(CapEff|CapPrm|CapBnd|NoNewPrivs):' /proc/self/status | tr -s '\\t ' ' ' | tr '\\n' ';'")

				for _, want := range []string{"CapEff: 0000000000000000", "CapPrm: 0000000000000000", "CapBnd: 0000000000000000", "NoNewPrivs: 1"} {
					if !strings.Contains(got, want) {
						t.Errorf("process status %q lacks %q", got, want)
					}
				}
			},
			"system_calls_are_filtered": func(t *testing.T) {
				if got := in(t, "grep '^Seccomp:' /proc/self/status | tr -s '\\t ' ' '"); got != "Seccomp: 2" {
					t.Errorf("%q, want seccomp in filter mode (2)", got)
				}
			},
			"the_root_filesystem_is_read_only": func(t *testing.T) {
				got := in(t, "for d in / /etc /usr /bin /root /home /var; do touch $d/escape-probe 2>/dev/null && echo wrote:$d; done; echo done")

				if got != "done" {
					t.Errorf("a path outside the workspace is writable: %q", got)
				}
			},
			"nothing_can_be_mounted": func(t *testing.T) {
				got := in(t, "mkdir -p /tmp/m; mount -t tmpfs none /tmp/m 2>/dev/null && echo mounted-tmpfs; "+
					"mount -t proc none /tmp/m 2>/dev/null && echo mounted-proc; "+
					"mount -o remount,rw / 2>/dev/null && echo remounted-root; echo done")

				if got != "done" {
					t.Errorf("a mount succeeded: %q", got)
				}
			},
			"kernel_settings_cannot_be_changed": func(t *testing.T) {
				got := in(t, "for f in /proc/sys/kernel/core_pattern /proc/sys/kernel/hostname /proc/sysrq-trigger /proc/sys/vm/drop_caches "+
					"/sys/kernel/uevent_helper /sys/fs/cgroup/cgroup.procs; do [ -e $f ] && echo x 2>/dev/null > $f && echo wrote:$f; done 2>/dev/null; echo done")

				if got != "done" {
					t.Errorf("a kernel setting is writable: %q", got)
				}
			},
			"host_disks_and_memory_are_not_exposed_as_devices": func(t *testing.T) {
				got := in(t, "ls /dev | grep -E '^(sd[a-z]|nvme|vd[a-z]|dm-|mem$|kmem$|kvm$|port$)' ; echo done")

				if got != "done" {
					t.Errorf("a host device is present in /dev: %q", got)
				}
			},
			"the_memory_limit_stops_a_process_that_allocates_without_bound": func(t *testing.T) {
				sb := newSandbox(t, shellImage)

				// /dev/shm and /workspace are memory-backed and bounded; filling
				// one must fail instead of taking memory from the host.
				res := sh(t, sb.ID, "dd if=/dev/zero of=/workspace/fill bs=1M count=4096 2>/dev/null; echo exit:$?")

				if strings.Contains(res.Stdout, "exit:0") {
					t.Errorf("writing 4 GiB to the workspace succeeded: %q", res.Stdout)
				}
				if got := sh(t, sb.ID, "echo alive"); strings.TrimSpace(got.Stdout) != "alive" {
					t.Errorf("the sandbox did not survive the attempt: %+v", got)
				}
			},
			"the_process_limit_contains_a_fork_bomb": func(t *testing.T) {
				bombed, neighbour := newSandbox(t, shellImage), newSandbox(t, shellImage)
				limit, err := strconv.Atoi(strings.TrimSpace(sh(t, bombed.ID, "cat /sys/fs/cgroup/pids.max").Stdout))
				if err != nil || limit <= 0 || limit >= 5000 {
					t.Fatalf("the sandbox has no process limit below 5000: pids.max = %d (%v)", limit, err)
				}

				// The answer to the bomb itself does not matter: the limit may cut
				// the command short, and it may leave the sandbox unable to start
				// another process until it is destroyed.
				call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+bombed.ID+"/executions", map[string]any{
					"cmd":        []string{"sh", "-c", "i=0; while [ $i -lt 5000 ]; do sleep 60 & i=$((i+1)); done 2>/dev/null; true"},
					"timeout_ms": 20000,
				})

				if got := sh(t, neighbour.ID, "echo alive"); strings.TrimSpace(got.Stdout) != "alive" {
					t.Errorf("another sandbox stopped answering after the fork bomb: %+v", got)
				}
				call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+bombed.ID, nil).want(t, http.StatusNoContent, "destroy the bombed sandbox")
			},
		}
		for name, fn := range sharedKernel {
			cases[name] = fn
		}
	}
	capability(t, "escape", nil, cases)
}
