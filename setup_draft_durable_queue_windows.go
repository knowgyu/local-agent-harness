//go:build windows

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const setupDraftQueueLockWait = 30 * time.Second

func withSetupDraftQueueLock(path string, fn func() error) error {
	if fn == nil {
		return errSetupDraftQueueUnavailable
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	security, attributes, err := setupDraftWindowsSecurityAttributes()
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	name := setupDraftQueueMutexName(path, security.userSID)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	handle, err := windows.CreateMutex(attributes, false, namePtr)
	if handle == 0 || (err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		return errSetupDraftQueueUnavailable
	}
	defer windows.CloseHandle(handle)
	if !setupDraftWindowsSecurityMatches(handle, windows.SE_KERNEL_OBJECT, security.userSID, true) {
		return errSetupDraftQueueUnavailable
	}
	wait, err := windows.WaitForSingleObject(handle, uint32(setupDraftQueueLockWait/time.Millisecond))
	if err != nil || (wait != windows.WAIT_OBJECT_0 && wait != windows.WAIT_ABANDONED) {
		return errSetupDraftQueueUnavailable
	}
	defer windows.ReleaseMutex(handle)
	if err := ensureSetupDraftQueueDirectory(path, security); err != nil {
		return err
	}
	return fn()
}

type setupDraftWindowsSecurity struct {
	userSID    string
	descriptor *windows.SECURITY_DESCRIPTOR
	attributes windows.SecurityAttributes
}

func setupDraftWindowsSecurityAttributes() (*setupDraftWindowsSecurity, *windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, nil, errSetupDraftQueueUnavailable
	}
	userSID := user.User.Sid.String()
	sddl := "O:" + userSID + "D:P(A;;FA;;;" + userSID + ")(A;;FA;;;SY)"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil || descriptor == nil {
		return nil, nil, errSetupDraftQueueUnavailable
	}
	security := &setupDraftWindowsSecurity{userSID: userSID, descriptor: descriptor}
	security.attributes = windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
		InheritHandle:      0,
	}
	return security, &security.attributes, nil
}

func setupDraftQueueMutexName(path, userSID string) string {
	canonical := strings.ToLower(filepath.Clean(path)) + "\x00" + userSID
	digest := sha256.Sum256([]byte(canonical))
	return `Local\LocalAgentHarness.SetupDraftQueue.v1.` + hex.EncodeToString(digest[:])
}

func ensureSetupDraftQueueDirectory(path string, security *setupDraftWindowsSecurity) error {
	if security == nil || !filepath.IsAbs(path) || filepath.VolumeName(path) == "" || strings.HasPrefix(path, `\\`) {
		return errSetupDraftQueueUnavailable
	}
	directory := filepath.Dir(filepath.Clean(path))
	if err := rejectSetupDraftWindowsReparseComponents(filepath.Dir(directory)); err != nil {
		return err
	}
	directoryPtr, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	err = windows.CreateDirectory(directoryPtr, &security.attributes)
	created := err == nil
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return errSetupDraftQueueUnavailable
	}
	handle, err := openSetupDraftWindowsObject(directory, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	defer windows.CloseHandle(handle)
	info, err := setupDraftWindowsHandleInfo(handle)
	if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errSetupDraftQueueUnavailable
	}
	if created && setSetupDraftWindowsObjectSecurity(handle, windows.SE_FILE_OBJECT, security.descriptor) != nil {
		return errSetupDraftQueueUnavailable
	}
	if !setupDraftWindowsSecurityMatches(handle, windows.SE_FILE_OBJECT, security.userSID, false) {
		return errSetupDraftQueueUnavailable
	}
	return nil
}

func rejectSetupDraftWindowsReparseComponents(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	if volume == "" || strings.HasPrefix(clean, `\\`) {
		return errSetupDraftQueueUnavailable
	}
	root := volume + string(filepath.Separator)
	current := root
	relative, err := filepath.Rel(root, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errSetupDraftQueueUnavailable
	}
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		handle, err := openSetupDraftWindowsObject(current, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
		if err != nil {
			return errSetupDraftQueueUnavailable
		}
		info, infoErr := setupDraftWindowsHandleInfo(handle)
		_ = windows.CloseHandle(handle)
		if infoErr != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return errSetupDraftQueueUnavailable
		}
	}
	return nil
}

func openSetupDraftWindowsObject(path string, access, flags uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, errSetupDraftQueueUnavailable
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	return handle, nil
}

func setupDraftWindowsHandleInfo(handle windows.Handle) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return windows.ByHandleFileInformation{}, err
	}
	return info, nil
}

func setupDraftWindowsSecurityMatches(handle windows.Handle, objectType windows.SE_OBJECT_TYPE, userSID string, kernelObject bool) bool {
	securityInfo := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	descriptor, err := windows.GetSecurityInfo(handle, objectType, securityInfo)
	if err != nil || descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || owner.String() != userSID {
		return false
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || dacl == nil || dacl.AceCount != 2 {
		return false
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	mask := "FA"
	if kernelObject {
		mask = "0x1f0001"
	}
	expected, err := windows.SecurityDescriptorFromString("O:" + userSID + "D:P(A;;" + mask + ";;;" + userSID + ")(A;;" + mask + ";;;SY)")
	if err != nil || expected == nil {
		return false
	}
	actualSDDL := strings.Replace(descriptor.String(), "D:PAI(", "D:P(", 1)
	return actualSDDL == expected.String()
}

func setSetupDraftWindowsObjectSecurity(handle windows.Handle, objectType windows.SE_OBJECT_TYPE, descriptor *windows.SECURITY_DESCRIPTOR) error {
	if descriptor == nil {
		return errSetupDraftQueueUnavailable
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errSetupDraftQueueUnavailable
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errSetupDraftQueueUnavailable
	}
	securityInfo := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	return windows.SetSecurityInfo(handle, objectType, securityInfo, owner, nil, dacl, nil)
}

func readSetupDraftQueueFile(path string, maxBytes int64) ([]byte, error) {
	security, _, err := setupDraftWindowsSecurityAttributes()
	if err != nil || ensureSetupDraftQueueDirectory(path, security) != nil {
		return nil, errSetupDraftQueueUnavailable
	}
	handle, err := openSetupDraftWindowsObject(path, windows.GENERIC_READ|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	info, err := setupDraftWindowsHandleInfo(handle)
	if err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 ||
		!setupDraftWindowsSecurityMatches(handle, windows.SE_FILE_OBJECT, security.userSID, false) {
		_ = windows.CloseHandle(handle)
		return nil, errSetupDraftQueueUnavailable
	}
	size := (uint64(info.FileSizeHigh) << 32) | uint64(info.FileSizeLow)
	if size == 0 || size > uint64(maxBytes) {
		_ = windows.CloseHandle(handle)
		return nil, errSetupDraftQueueInvalid
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errSetupDraftQueueUnavailable
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) != int64(size) || int64(len(data)) > maxBytes {
		return nil, errSetupDraftQueueInvalid
	}
	return data, nil
}

func writeSetupDraftQueueFileAtomically(path string, data []byte) error {
	security, attributes, err := setupDraftWindowsSecurityAttributes()
	if err != nil || ensureSetupDraftQueueDirectory(path, security) != nil {
		return errSetupDraftQueueUnavailable
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errSetupDraftQueueUnavailable
	}
	tempPath := path + "." + hex.EncodeToString(random[:]) + ".tmp"
	tempName, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	tempHandle, err := windows.CreateFile(tempName, windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, 0, attributes, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	file := os.NewFile(uintptr(tempHandle), tempPath)
	if file == nil {
		_ = windows.CloseHandle(tempHandle)
		_ = os.Remove(tempPath)
		return errSetupDraftQueueUnavailable
	}
	if err := setSetupDraftWindowsObjectSecurity(tempHandle, windows.SE_FILE_OBJECT, security.descriptor); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return errSetupDraftQueueUnavailable
	}
	keep := false
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := writeFullQueueData(file, data); err != nil {
		return errSetupDraftQueueUnavailable
	}
	if err := file.Sync(); err != nil {
		return errSetupDraftQueueUnavailable
	}
	if err := file.Close(); err != nil {
		return errSetupDraftQueueUnavailable
	}
	file = nil
	tempHandle, err = openSetupDraftWindowsObject(tempPath, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	info, infoErr := setupDraftWindowsHandleInfo(tempHandle)
	secure := infoErr == nil && info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) == 0 &&
		setupDraftWindowsSecurityMatches(tempHandle, windows.SE_FILE_OBJECT, security.userSID, false)
	_ = windows.CloseHandle(tempHandle)
	if !secure {
		return errSetupDraftQueueUnavailable
	}
	if err := windows.Rename(tempPath, path); err != nil {
		return errSetupDraftQueueUnavailable
	}
	keep = true
	if _, err := readSetupDraftQueueFile(path, setupDraftQueueMaxFileBytes); err != nil {
		return errSetupDraftQueueUnavailable
	}
	return nil
}

func writeFullQueueData(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
