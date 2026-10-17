package cloud

import (
	"strings"
	"testing"
)

func TestCloudInitQuotesBootstrap(t *testing.T) {
	ci := CloudInit(Boot{Packages: []string{"docker.io"}, Files: map[string]string{"/opt/spade/install.sh": "#!/bin/sh\necho \"hi\"\n"},
		Pre: []string{"echo pre"}, Run: `SPADE_X='1' sh /opt/spade/install.sh`})
	for _, want := range []string{"packages:\n  - \"docker.io\"", `content: "#!/bin/sh\necho \"hi\"\n"`, `- [ sh, -c, "echo pre" ]`, `- [ sh, -c, "SPADE_X='1' sh /opt/spade/install.sh" ]`} {
		if !strings.Contains(ci, want) {
			t.Fatalf("cloud-init missing %q:\n%s", want, ci)
		}
	}
	if strings.Index(ci, "echo pre") > strings.Index(ci, "install.sh\" ]") {
		t.Fatalf("pre steps must run before the bootstrap:\n%s", ci)
	}
}

func TestFromTagsNeedsServiceTag(t *testing.T) {
	if a, ok := FromTags([]string{ServiceTag("s1"), AttemptTag("a1"), ManagedTag}, "s1"); !ok || a != "a1" {
		t.Fatalf("got %q %v", a, ok)
	}
	if _, ok := FromTags([]string{ServiceTag("s10"), AttemptTag("a1")}, "s1"); ok {
		t.Fatal("a different service's tag must not match")
	}
}
