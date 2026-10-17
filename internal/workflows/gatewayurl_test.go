package workflows

import "testing"

func TestSafeGatewayURL(t *testing.T) {
	for _, u := range []string{"https://gw.spadeai.ng", "https://gw.spadeai.ng:8443", "https://10.0.0.1:8443/agent"} {
		if !safeGatewayURL.MatchString(u) {
			t.Errorf("%s should be accepted", u)
		}
	}
	for _, u := range []string{"http://gw", "https://gw'; reboot; '", "https://gw $(id)", "https://gw\nx", ""} {
		if safeGatewayURL.MatchString(u) {
			t.Errorf("%q must be rejected", u)
		}
	}
}
