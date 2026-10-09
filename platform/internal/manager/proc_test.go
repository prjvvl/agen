package manager

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestAllocHelper is not a test: run as a child process by
// TestHostMemoryLimitIsEnforced, it waits a moment (so the limit is in
// place) and then touches 512 MiB.
func TestAllocHelper(t *testing.T) {
	if os.Getenv("AGEN_ALLOC_HELPER") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(time.Second)
	buf := make([]byte, 512<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	os.Exit(0)
}

// A native host over its memory limit is stopped by the OS.
func TestHostMemoryLimitIsEnforced(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("memory limits are enforced on Windows and Linux")
	}
	run := func(limit uint64) error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAllocHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AGEN_ALLOC_HELPER=1")
		prepareCmd(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if !afterStart(cmd, limit) {
			_ = cmd.Process.Kill()
			t.Fatalf("limit %d not applied", limit)
		}
		return cmd.Wait()
	}
	if err := run(0); err != nil {
		t.Fatalf("without a limit the helper should finish: %v", err)
	}
	if err := run(200 << 20); err == nil {
		t.Fatal("a 512 MiB allocation succeeded under a 200 MiB limit")
	}
}
