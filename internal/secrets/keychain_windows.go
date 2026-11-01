//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric            = 1
	credPersistLocalMachine    = 2
	credMaximumBlobSize        = 5 * 512
	credentialManagerUserLabel = "masamong"
)

type credentialAttribute struct {
	Keyword   *uint16
	Flags     uint32
	ValueSize uint32
	Value     *byte
}

type windowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         *credentialAttribute
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	credWriteProc  = windows.NewLazySystemDLL("advapi32.dll").NewProc("CredWriteW")
	credReadProc   = windows.NewLazySystemDLL("advapi32.dll").NewProc("CredReadW")
	credDeleteProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("CredDeleteW")
	credFreeProc   = windows.NewLazySystemDLL("advapi32.dll").NewProc("CredFree")
)

func init() {
	windowsCredentialRead = credentialManagerRead
	windowsCredentialWrite = credentialManagerWrite
	windowsCredentialDelete = credentialManagerDelete
}

func windowsCredentialTarget(account string) (*uint16, error) {
	return windows.UTF16PtrFromString(KeychainService + "/" + account)
}

func credentialManagerRead(account string) (string, error) {
	target, err := windowsCredentialTarget(account)
	if err != nil {
		return "", err
	}
	var found *windowsCredential
	result, _, callErr := credReadProc.Call(
		uintptr(unsafe.Pointer(target)), uintptr(credTypeGeneric), 0,
		uintptr(unsafe.Pointer(&found)),
	)
	runtime.KeepAlive(target)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return "", nil
		}
		return "", fmt.Errorf("keychain: Credential Manager read failed: %w", callErr)
	}
	if found == nil {
		return "", fmt.Errorf("keychain: Credential Manager returned no credential")
	}
	defer credFreeProc.Call(uintptr(unsafe.Pointer(found)))
	if found.CredentialBlobSize == 0 {
		return "", nil
	}
	if found.CredentialBlob == nil || found.CredentialBlobSize > credMaximumBlobSize {
		return "", fmt.Errorf("keychain: Credential Manager returned an invalid credential")
	}
	return string(unsafe.Slice(found.CredentialBlob, found.CredentialBlobSize)), nil
}

func credentialManagerWrite(account, value string) error {
	blob := []byte(value)
	if len(blob) > credMaximumBlobSize {
		return fmt.Errorf("%w: Credential Manager values are limited to %d bytes", ErrKeychainValueTooLong, credMaximumBlobSize)
	}
	target, err := windowsCredentialTarget(account)
	if err != nil {
		return err
	}
	user, err := windows.UTF16PtrFromString(credentialManagerUserLabel)
	if err != nil {
		return err
	}
	credential := windowsCredential{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		Persist:            credPersistLocalMachine,
		UserName:           user,
	}
	if len(blob) > 0 {
		credential.CredentialBlob = &blob[0]
	}
	result, _, callErr := credWriteProc.Call(uintptr(unsafe.Pointer(&credential)), 0)
	runtime.KeepAlive(target)
	runtime.KeepAlive(user)
	runtime.KeepAlive(blob)
	runtime.KeepAlive(credential)
	if result == 0 {
		return fmt.Errorf("keychain: Credential Manager write failed: %w", callErr)
	}
	got, err := credentialManagerRead(account)
	if err != nil {
		return err
	}
	if got != value {
		return errors.New("keychain: Credential Manager write could not be verified")
	}
	return nil
}

func credentialManagerDelete(account string) error {
	target, err := windowsCredentialTarget(account)
	if err != nil {
		return err
	}
	result, _, callErr := credDeleteProc.Call(uintptr(unsafe.Pointer(target)), uintptr(credTypeGeneric), 0)
	runtime.KeepAlive(target)
	if result == 0 && !errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("keychain: Credential Manager delete failed: %w", callErr)
	}
	return nil
}
