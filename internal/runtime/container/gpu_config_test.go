package container

import "testing"

// M19: a GPU spec requests exactly ONE nvidia GPU (not all GPUs on the node — review H1).
func TestHardenedConfig_gpu_requests_one_device(t *testing.T) {
	_, host := hardenedConfig(Spec{SandboxID: "s", Image: "img", GPU: "a100"})
	if len(host.DeviceRequests) != 1 {
		t.Fatalf("DeviceRequests = %d, want 1", len(host.DeviceRequests))
	}
	dr := host.DeviceRequests[0]
	if dr.Driver != "nvidia" || dr.Count != 1 {
		t.Errorf("DeviceRequest = {driver:%q count:%d}, want {nvidia 1}", dr.Driver, dr.Count)
	}
	// A non-GPU spec requests no devices.
	_, host2 := hardenedConfig(Spec{SandboxID: "s", Image: "img"})
	if len(host2.DeviceRequests) != 0 {
		t.Errorf("non-GPU DeviceRequests = %d, want 0", len(host2.DeviceRequests))
	}
}
