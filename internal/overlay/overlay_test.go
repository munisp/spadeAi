package overlay

import "testing"

var peers = []Peer{
	{ServiceID: "bbbbbbbb-2", IP: "10.42.0.3", PublicKey: "kb", Endpoint: "192.0.2.2:51820", HostCert: "cb"},
	{ServiceID: "aaaaaaaa-1", IP: "10.42.0.2", PublicKey: "ka", Endpoint: "192.0.2.1:51820", HostCert: "ca"},
}

func TestWireGuardFullMeshExcludesSelf(t *testing.T) {
	out, err := Payloads("wireguard", "12345678-p", "10.42.0.0/16", 7, "", peers)
	if err != nil {
		t.Fatal(err)
	}
	a := out["aaaaaaaa-1"]
	if a["address"] != "10.42.0.2/16" || a["iface"] != "spw-12345678" || a["generation"] != int64(7) {
		t.Fatalf("bad payload %v", a)
	}
	ps := a["peers"].([]map[string]any)
	if len(ps) != 1 || ps[0]["public_key"] != "kb" || ps[0]["allowed_ips"] != "10.42.0.3/32" {
		t.Fatalf("bad peers %v", ps)
	}
}

func TestNebulaLowestIPIsLighthouse(t *testing.T) {
	out, err := Payloads("nebula", "12345678-p", "10.42.0.0/16", 1, "CA", peers)
	if err != nil {
		t.Fatal(err)
	}
	if out["aaaaaaaa-1"]["am_lighthouse"] != true || out["bbbbbbbb-2"]["am_lighthouse"] != false {
		t.Fatal("wrong lighthouse")
	}
	lh := out["bbbbbbbb-2"]["lighthouses"].([]string)
	if len(lh) != 1 || lh[0] != "10.42.0.2" {
		t.Fatalf("lighthouses %v", lh)
	}
	if m := out["bbbbbbbb-2"]["static_host_map"].(map[string][]string); m["10.42.0.2"][0] != "192.0.2.1:51820" {
		t.Fatalf("static_host_map %v", m)
	}
}

func TestNebulaRequiresSignedCerts(t *testing.T) {
	if _, err := Payloads("nebula", "12345678-p", "10.42.0.0/16", 1, "", peers); err == nil {
		t.Fatal("expected error without CA")
	}
}
