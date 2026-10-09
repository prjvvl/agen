package manager

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Instance processes join a Job Object that kills them when the Manager
// exits, so a crashed Manager leaves no orphan hosts. With a memory limit,
// the job also caps each process's committed memory (one job per limit).
var (
	jobMu sync.Mutex
	jobs  = map[uint64]windows.Handle{}
)

func managerJob(memLimit uint64) windows.Handle {
	jobMu.Lock()
	defer jobMu.Unlock()
	if h, ok := jobs[memLimit]; ok {
		return h
	}
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if memLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(memLimit)
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return 0
	}
	jobs[memLimit] = h
	return h
}

func prepareCmd(*exec.Cmd) {}

// afterStart puts the host in the Manager's job (and so under its memory
// limit, if any). It reports whether a requested limit is enforced; without
// a limit, joining the job is best effort.
func afterStart(cmd *exec.Cmd, memLimit uint64) bool {
	ok := func() bool {
		j := managerJob(memLimit)
		if j == 0 {
			return false
		}
		p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			return false
		}
		defer windows.CloseHandle(p)
		return windows.AssignProcessToJobObject(j, p) == nil
	}()
	return ok || memLimit == 0
}

// terminate asks the process to stop. Windows has no SIGTERM for console
// children, so the Manager drains the host first and then kills it.
func terminate(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
