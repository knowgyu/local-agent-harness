//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const clientRegistrationPrivateFileAccessMask windows.ACCESS_MASK = 0x001F01FF

func ensureClientRegistrationBackupDirectory(path string) error {
	if err := rejectClientRegistrationReparseAncestors(path); err != nil {
		return err
	}
	if err := applyClientRegistrationPrivateACL(path); err != nil {
		return err
	}
	return validateClientRegistrationBackupStoragePath(path, true)
}

func secureClientRegistrationBackupFile(path string) error {
	if err := rejectClientRegistrationReparseAncestors(path); err != nil {
		return err
	}
	if err := applyClientRegistrationPrivateACL(path); err != nil {
		return err
	}
	return validateClientRegistrationBackupStoragePath(path, false)
}

func validateClientRegistrationBackupStoragePath(path string, directory bool) error {
	if err := rejectClientRegistrationReparseAncestors(path); err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(filepath.Clean(path)))
	if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return os.ErrPermission
	}
	if directory && attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || !directory && attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return os.ErrPermission
	}
	return verifyClientRegistrationPrivateACL(path, 0)
}

func openClientRegistrationBackupFile(path string) (*os.File, error) {
	if err := validateClientRegistrationBackupStoragePath(path, false); err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(filepath.Clean(path)),
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrPermission
	}
	if err := verifyClientRegistrationPrivateACL(path, handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrPermission
	}
	return file, nil
}

func rejectClientRegistrationReparseAncestors(path string) error {
	cleaned := filepath.Clean(path)
	volume := filepath.VolumeName(cleaned)
	if volume == "" {
		return os.ErrPermission
	}
	root := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(cleaned, root)
	current := root
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
		if err != nil {
			if errorsIsNotExist(err) {
				continue
			}
			return err
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return os.ErrPermission
		}
	}
	return nil
}

func errorsIsNotExist(err error) bool {
	return err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND
}

func applyClientRegistrationPrivateACL(path string) error {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return os.ErrPermission
	}
	entries := []windows.EXPLICIT_ACCESS{privateClientRegistrationACE(tokenUser.User.Sid, windows.TRUSTEE_IS_USER)}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func privateClientRegistrationACE(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func verifyClientRegistrationPrivateACL(path string, handle windows.Handle) error {
	var descriptor *windows.SECURITY_DESCRIPTOR
	var err error
	securityInfo := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if handle != 0 {
		descriptor, err = windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, securityInfo)
	} else {
		descriptor, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, securityInfo)
	}
	if err != nil || descriptor == nil {
		return os.ErrPermission
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || currentUser == nil || currentUser.User.Sid == nil {
		return os.ErrPermission
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(currentUser.User.Sid) {
		return os.ErrPermission
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return os.ErrPermission
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return os.ErrPermission
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != clientRegistrationPrivateFileAccessMask {
		return os.ErrPermission
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.Equals(currentUser.User.Sid) {
		return os.ErrPermission
	}
	return nil
}
