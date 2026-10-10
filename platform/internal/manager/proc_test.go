package manager

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestAllocHelper is not a test: run as a child process by
// TestHostMemoryLimitIsEnforced, it waits a moment (so the limit is in
// place), touches 512 MiB and reports that it got it.
func TestAllocHelper(t *testing.T) {
	if os.Getenv("AGEN_ALLOC_HELPER") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(time.Second)
	buf := make([]byte, 512<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	os.Stdout.WriteString("allocated\n")
	os.Exit(0)
}

// A native host over its memory limit cannot get the memory. Over the limit
// the helper usually dies, but the Go runtime can also stall retrying the
// allocation, so a helper that never reports success counts as limited.
func TestHostMemoryLimitIsEnforced(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("memory limits are enforced on Windows and Linux")
	}
	allocated := func(limit uint64) bool {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAllocHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AGEN_ALLOC_HELPER=1")
		var out bytes.Buffer
		cmd.Stdout = &out
		prepareCmd(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if !afterStart(cmd, limit) {
			_ = cmd.Process.Kill()
			t.Fatalf("limit %d not applied", limit)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return strings.Contains(out.String(), "allocated")
	}
	if !allocated(0) {
		t.Fatal("without a limit the helper should get 512 MiB")
	}
	if allocated(200 << 20) {
		t.Fatal("a 512 MiB allocation succeeded under a 200 MiB limit")
	}
}
