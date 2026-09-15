package control

import (
	"net/http"
	"testing"
)

// An agent that predates linked pairs must refuse a pair rental outright: the
// control plane treats anything but a 200 from /pair/provision as a refusal,
// while /provision on such an agent silently ignores the pair fields and would
// boot a lone VM. This test was written and run against control.New as it was
// before the pair endpoints existed (agent v0.1.8, commit 5600a1d), and it keeps
// holding for any Provisioner that does not implement the pair runtime.
func TestPairProvisionIsUnknownToAnAgentWithoutThePairRuntime(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	body := `{"rental_id":"1a2b3c4d-0000-4000-8000-000000000000","renter_pubkey":"k","node":"a",` +
		`"peer_hostname":"gpu-1a2b3c4d-b","links":[{"local_mac":"58:a2:e1:00:00:01","cidr":"10.200.0.1/30","peer_ip":"10.200.0.2"}],"mtu":9000}`
	for _, path := range []string{"/pair/provision", "/link/verify"} {
		w := serve(s, "POST", path, body, "secret")
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404 from an agent without the pair runtime", path, w.Code)
		}
	}
}
