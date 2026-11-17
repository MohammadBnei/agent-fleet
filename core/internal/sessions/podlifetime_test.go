package sessions

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// PodLifetime copies the provisioner's podLifetime (another module); this fails if they drift apart.
func TestPodLifetimeMatchesProvisioner(t *testing.T) {
	src, err := os.ReadFile("../../../provisioner/internal/grpcserver/server.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const podLifetime = (\d+) \* time\.Hour`).FindSubmatch(src)
	if m == nil {
		t.Fatal("provisioner podLifetime not found; update this test")
	}
	h, _ := strconv.Atoi(string(m[1]))
	if time.Duration(h)*time.Hour != PodLifetime {
		t.Fatalf("provisioner podLifetime %dh != core PodLifetime %v", h, PodLifetime)
	}
}
