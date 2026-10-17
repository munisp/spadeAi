package cloud

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestScriptRunsBootInOrder(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	f := filepath.Join(dir, "it's here.sh")
	sc := Script(Boot{Files: map[string]string{f: "echo \"file $1\" >> " + out + "\n"},
		Pre: []string{"echo pre > " + out}, Run: "sh '" + strings.ReplaceAll(f, "'", `'"'"'`) + "' 'a;b'"})
	cmd := exec.Command("sh", "-c", sc)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v %s\n%s", err, b, sc)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "pre\nfile a;b\n" {
		t.Fatalf("got %q", got)
	}
	if fail := Script(Boot{Pre: []string{"false"}, Run: "touch " + filepath.Join(dir, "ran")}); exec.Command("sh", "-c", fail).Run() == nil {
		t.Fatal("a failing step must stop the script")
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Fatal("steps after a failure must not run")
	}
}
