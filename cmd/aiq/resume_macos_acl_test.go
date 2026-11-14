package main

import (
	"encoding/binary"
	"testing"
)

func fixtureDarwinACL(entries ...[2]uint32) []byte {
	data := make([]byte, 44+24*len(entries))
	binary.NativeEndian.PutUint32(data, 0x012cc16d)
	binary.NativeEndian.PutUint32(data[36:40], uint32(len(entries)))
	for i, entry := range entries {
		base := 44 + 24*i
		binary.NativeEndian.PutUint32(data[base+16:base+20], entry[0])
		binary.NativeEndian.PutUint32(data[base+20:base+24], entry[1])
	}
	return data
}

func TestResumeDarwinACLPermissionClass(t *testing.T) {
	for _, bit := range []uint{2, 4, 5, 6, 8, 10, 12, 13, 21, 23} {
		for _, inherit := range []uint32{0, 1 << 8} {
			data := fixtureDarwinACL([2]uint32{1 | inherit, 1 << bit})
			if write, err := resumeDarwinACLGrantsWrite(data); err != nil || !write {
				t.Fatalf("write grant bit %d ignored: %v", bit, err)
			}
		}
	}
	for _, data := range [][]byte{nil, fixtureDarwinACL(), fixtureDarwinACL([2]uint32{2, resumeDarwinWriteRights}), fixtureDarwinACL([2]uint32{1, resumeDarwinReadRights}, [2]uint32{2, 1 << 4})} {
		if write, err := resumeDarwinACLGrantsWrite(data); err != nil || write {
			t.Fatal("absent/deny/read ACL refused", write, err)
		}
	}
	noACL := fixtureDarwinACL()
	binary.NativeEndian.PutUint32(noACL[36:40], 0xffffffff)
	if write, err := resumeDarwinACLGrantsWrite(noACL); err != nil || write {
		t.Fatal("NOACL sentinel misread", write, err)
	}
	entries := make([][2]uint32, 128)
	for i := range entries {
		entries[i] = [2]uint32{2, 1 << 4}
	}
	if write, err := resumeDarwinACLGrantsWrite(fixtureDarwinACL(entries...)); err != nil || write {
		t.Fatal("kernel maximum unsupported", err)
	}
}

func TestResumeDarwinACLMalformedClass(t *testing.T) {
	for _, tc := range []string{"short header", "bad magic", "bad count", "extra payload", "short entry", "unknown ACL flags", "unknown entry flags", "unknown kind", "unknown rights", "oversize", "NOACL with data"} {
		t.Run(tc, func(t *testing.T) {
			data := fixtureDarwinACL([2]uint32{1, 1 << 1})
			switch tc {
			case "short header":
				data = data[:43]
			case "bad magic":
				data[0] ^= 1
			case "bad count":
				binary.NativeEndian.PutUint32(data[36:40], 129)
			case "extra payload":
				data = append(data, 0)
			case "short entry":
				data = data[:len(data)-1]
			case "unknown ACL flags":
				binary.NativeEndian.PutUint32(data[40:44], 1<<18)
			case "unknown entry flags":
				binary.NativeEndian.PutUint32(data[60:64], 1|1<<12)
			case "unknown kind":
				binary.NativeEndian.PutUint32(data[60:64], 3)
			case "unknown rights":
				binary.NativeEndian.PutUint32(data[64:68], 1<<31)
			case "oversize":
				data = make([]byte, resumeDarwinACLBytes+1)
			case "NOACL with data":
				binary.NativeEndian.PutUint32(data[36:40], 0xffffffff)
			}
			if _, err := resumeDarwinACLGrantsWrite(data); err == nil {
				t.Fatal("malformed metadata approved")
			}
		})
	}
}

func FuzzResumeDarwinACL(f *testing.F) {
	f.Add([]byte{})
	f.Add(fixtureDarwinACL())
	f.Add(fixtureDarwinACL([2]uint32{1, 1 << 6}))
	f.Add(fixtureDarwinACL([2]uint32{2, 1 << 4}))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = resumeDarwinACLGrantsWrite(data) })
}
