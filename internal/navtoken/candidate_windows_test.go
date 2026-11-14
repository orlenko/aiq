// Native candidate retained only for future platform validation.
package navtoken

import (
	"io"
	"os"
	"syscall"
)

const fileReadAttributes = 0x80

type fileIdentity struct{ volume, high, low uint32 }

func identity(handle syscall.Handle) (fileIdentity, error) {
	kind, err := syscall.GetFileType(handle)
	if err != nil {
		return fileIdentity{}, ErrUnavailable
	}
	var facts syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(handle, &facts) != nil {
		return fileIdentity{}, ErrUnavailable
	}
	if kind != syscall.FILE_TYPE_DISK || facts.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fileIdentity{}, ErrUnsafe
	}
	return fileIdentity{facts.VolumeSerialNumber, facts.FileIndexHigh, facts.FileIndexLow}, nil
}

type windowsReader struct {
	*os.File
	metadata syscall.Handle
}

func (f *windowsReader) Close() error {
	dataErr := f.File.Close()
	metadataErr := syscall.CloseHandle(f.metadata)
	if dataErr != nil || metadataErr != nil {
		return ErrUnavailable
	}
	return nil
}

// This test-only seam schedules invented native replacement fixtures.
// Production Read never calls this candidate.
func openWindows(path string, afterMetadata func()) (out io.ReadCloser, err error) {
	name, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return nil, ErrUnavailable
	}
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)
	flags := uint32(syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.FILE_FLAG_BACKUP_SEMANTICS)
	metadata, e := syscall.CreateFile(name, fileReadAttributes, share, nil, syscall.OPEN_EXISTING, flags, 0)
	if e != nil {
		return nil, ErrUnavailable
	}
	data := syscall.InvalidHandle
	var file *os.File
	defer func() {
		if out != nil {
			return
		} // both handles transferred to windowsReader
		if file != nil {
			file.Close()
		} else if data != syscall.InvalidHandle {
			syscall.CloseHandle(data)
		}
		syscall.CloseHandle(metadata)
	}()
	a, e := identity(metadata)
	if e != nil {
		return nil, e
	}
	if afterMetadata != nil {
		afterMetadata()
	}
	data, e = syscall.CreateFile(name, syscall.GENERIC_READ, share, nil, syscall.OPEN_EXISTING, flags, 0)
	if e != nil {
		return nil, ErrUnavailable
	}
	b, e := identity(data)
	if e != nil {
		return nil, e
	}
	if a != b {
		return nil, ErrUnsafe
	}
	file = os.NewFile(uintptr(data), "navigator-state")
	if file == nil {
		return nil, ErrUnavailable
	}
	data = syscall.InvalidHandle
	return &windowsReader{File: file, metadata: metadata}, nil
}

func readWindowsCandidate(path string) (Record, error) {
	f, err := openWindows(path, nil)
	if err != nil {
		return Record{}, err
	}
	return readFile(f)
}
