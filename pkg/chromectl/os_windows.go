//go:build windows

package chromectl

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pathIDBytes is the path Chrome hashes for an unpacked extension id.
// On Windows that is the UTF-16LE path, with the drive letter uppercased.
func pathIDBytes(path string) []byte {
	if len(path) >= 2 && path[0] >= 'a' && path[0] <= 'z' && path[1] == ':' {
		path = string(path[0]-'a'+'A') + path[1:]
	}
	units := utf16.Encode([]rune(path))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[i*2] = byte(u)
		b[i*2+1] = byte(u >> 8)
	}
	return b
}

func chromePlatformArgs() []string { return nil }

func chromeSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func (c *Controller) chromePIDs() []int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var ent windows.ProcessEntry32
	ent.Size = uint32(unsafe.Sizeof(ent))
	if err := windows.Process32First(snap, &ent); err != nil {
		return nil
	}
	flag := "--user-data-dir=" + c.UserDataDir
	var out []int
	for {
		name := windows.UTF16ToString(ent.ExeFile[:])
		if strings.EqualFold(name, "chrome.exe") {
			cmd := processCommandLine(ent.ProcessID)
			if chromeBrowserMatch(cmd, flag) {
				out = append(out, int(ent.ProcessID))
			}
		}
		if err := windows.Process32Next(snap, &ent); err != nil {
			break
		}
	}
	return out
}

func chromeBrowserMatch(cmd, userDataFlag string) bool {
	if cmd == "" || strings.Contains(cmd, "--type=") {
		return false
	}
	i := strings.Index(cmd, userDataFlag)
	if i < 0 {
		return false
	}
	end := i + len(userDataFlag)
	if end >= len(cmd) {
		return true
	}
	switch cmd[end] {
	case ' ', '"', '\t', '\r', '\n':
		return true
	default:
		// chrome-profile must not match chrome-profile-ports or a longer path.
		return false
	}
}

func linkDir(link, target string) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.Mkdir(link, 0o700); err != nil {
		return err
	}
	made := true
	defer func() {
		if made {
			_ = os.Remove(link)
		}
	}()
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := setMountPoint(h, target); err != nil {
		return err
	}
	made = false
	return nil
}

// setMountPoint turns an empty directory into a junction to target.
func setMountPoint(h windows.Handle, target string) error {
	sub := utf16.Encode([]rune(`\??\` + target))
	printName := utf16.Encode([]rune(target))
	bufBytes := (len(sub) + 1 + len(printName) + 1) * 2
	raw := make([]byte, 16+bufBytes)
	binary.LittleEndian.PutUint32(raw[0:], 0xA0000003) // IO_REPARSE_TAG_MOUNT_POINT
	binary.LittleEndian.PutUint16(raw[4:], uint16(8+bufBytes))
	binary.LittleEndian.PutUint16(raw[10:], uint16(len(sub)*2))
	binary.LittleEndian.PutUint16(raw[12:], uint16((len(sub)+1)*2))
	binary.LittleEndian.PutUint16(raw[14:], uint16(len(printName)*2))
	off := 16
	for _, u := range sub {
		binary.LittleEndian.PutUint16(raw[off:], u)
		off += 2
	}
	off += 2
	for _, u := range printName {
		binary.LittleEndian.PutUint16(raw[off:], u)
		off += 2
	}
	var n uint32
	return windows.DeviceIoControl(h, 0x000900A4, &raw[0], uint32(len(raw)), nil, 0, &n, nil)
}

func processCommandLine(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		h, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return ""
		}
	}
	defer windows.CloseHandle(h)
	var needed uint32
	_ = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, nil, 0, &needed)
	if needed < 8 {
		needed = 1 << 16
	}
	buf := make([]byte, needed)
	err = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &needed)
	if err != nil {
		return ""
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Buffer == nil || us.Length == 0 {
		return ""
	}
	return windows.UTF16PtrToString(us.Buffer)
}

func stopPIDs(ctx context.Context, pids []int) error {
	var handles []windows.Handle
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			continue
		}
		_ = windows.TerminateProcess(h, 1)
		handles = append(handles, h)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, h := range handles {
		wait := time.Until(deadline)
		if wait < 0 {
			wait = 0
		}
		_, _ = windows.WaitForSingleObject(h, uint32(wait.Milliseconds()))
		windows.CloseHandle(h)
	}
	return sleep(ctx, 300*time.Millisecond)
}

func (c *Controller) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(c.UserDataDir, ".uploader.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		var ol windows.Overlapped
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
		if err == nil {
			return func() {
				var ol windows.Overlapped
				_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
				f.Close()
			}, nil
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			f.Close()
			return nil, err
		}
	}
}
