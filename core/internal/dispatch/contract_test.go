package dispatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Core acks and drops events the bridge flags uncontracted, so a contract that
// loses a type core acts on would silently lose work. This fails at re-pin
// time instead: every type and prefix core routes on must be in the pinned
// bridge contract.
func TestPinnedContractCoversRoutedTypes(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/MohammadBnei/ukubi-bridge").Output()
	if err != nil {
		t.Skipf("bridge module not resolvable: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "contract", "events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := string(b)
	for _, typ := range []string{typeAlertFiring, typeAlertResolved, typePRClosed,
		typeRun + "success", typeRun + "failure", typeRun + "recover"} {
		if !strings.Contains(c, "type: "+typ+",") {
			t.Errorf("bridge contract has no %s", typ)
		}
	}
	for _, p := range Types {
		if !strings.Contains(c, "type: "+p) && !strings.Contains(c, `type: "`+p) {
			t.Errorf("bridge contract has nothing under subscribed prefix %s", p)
		}
	}
}
