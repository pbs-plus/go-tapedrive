package tapedrive

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenSetsCloseOnExec(t *testing.T) {
	d, err := Open("/dev/null")
	if err != nil {
		t.Skipf("cannot open /dev/null: %v", err)
	}
	defer d.Close()

	fd := int(d.f.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("tape fd %d is NOT close-on-exec (flags=%d); children will inherit it", fd, flags)
	}
}

func TestOpenFdNotInherited(t *testing.T) {
	tmp, err := os.CreateTemp("", "cloexec-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	fd, err := unix.Open(tmp.Name(), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)

	cmd := exec.Command("ls", "-l", "/proc/self/fd")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child exec: %v", err)
	}
	_ = out

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("fd %d not close-on-exec", fd)
	}
}
