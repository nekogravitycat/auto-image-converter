package convert

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// killOnCloseJob is a Windows job object configured so that every process
// assigned to it is terminated when the job handle is closed. The handle closes
// when this program exits for any reason — a clean shutdown, a panic, or a hard
// kill — so no HEIF worker can outlive the parent, not even one wedged inside an
// encode and thus deaf to the stdin-EOF that normally tells it to quit.
type killOnCloseJob struct {
	handle windows.Handle
}

// newKillOnCloseJob creates the job object and applies the kill-on-close limit.
func newKillOnCloseJob() (*killOnCloseJob, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &killOnCloseJob{handle: h}, nil
}

// assign adds the already-running process with the given PID to the job.
func (j *killOnCloseJob) assign(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(j.handle, h)
}

// close closes the job handle, which terminates every assigned process still
// running. Safe to call more than once.
func (j *killOnCloseJob) close() {
	if j.handle != 0 {
		_ = windows.CloseHandle(j.handle)
		j.handle = 0
	}
}
