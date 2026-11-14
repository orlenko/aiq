package main

import (
	"encoding/binary"
	"errors"
)

// Darwin's kauth_filesec header is 44 bytes and each ACE is 24. The kernel
// ABI limits a list to 128 entries; these are format bounds, not policy limits.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/kauth.h
const resumeDarwinACLBytes = 44 + 24*128
const resumeDarwinWriteRights uint32 = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
const resumeDarwinReadRights uint32 = 1<<1 | 1<<3 | 1<<7 | 1<<9 | 1<<11 | 1<<20 | 1<<22 | 1<<24

func resumeDarwinACLGrantsWrite(data []byte) (bool, error) {
	bad := func() (bool, error) { return false, errors.New("unsupported macOS ACL metadata") }
	if len(data) == 0 {
		return false, nil
	}
	if len(data) < 44 || len(data) > resumeDarwinACLBytes {
		return bad()
	}
	get := binary.NativeEndian.Uint32
	if get(data[:4]) != 0x012cc16d {
		return bad()
	}
	count := get(data[36:40])
	flags := get(data[40:44])
	if flags & ^uint32(0x3ffff) != 0 {
		return bad()
	}
	if count == 0xffffffff {
		if len(data) != 44 {
			return bad()
		}
		return false, nil
	}
	if count > 128 || len(data) != 44+24*int(count) {
		return bad()
	}
	for i := 0; i < int(count); i++ {
		entry := data[44+i*24 : 44+(i+1)*24]
		flags := get(entry[16:20])
		rights := get(entry[20:24])
		kind := flags & 15
		if kind != 1 && kind != 2 || flags & ^uint32(0x7ff) != 0 || rights & ^uint32(resumeDarwinWriteRights|resumeDarwinReadRights) != 0 {
			return bad()
		}
		// A deny or read-only grant cannot introduce another writer. We do
		// not infer that a write grant's principal belongs to the caller.
		if kind == 1 && rights&resumeDarwinWriteRights != 0 {
			return true, nil
		}
	}
	return false, nil
}
