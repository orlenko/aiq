package tmux

import (
	"strings"
	"testing"
)

func TestAttachMetadataLiteralIdentity(t *testing.T) {
	for _, socket := range []string{"/fixture/socket", "/fixture/ comma,pipe|tab\t space ", "/fixture/ends-with-newline\n"} {
		info, err := parseAttachInfo([]byte("%17|2468|0|$3|@9|"+socket+"\n"), "%17", 2468)
		if err != nil || info.pane != "%17" || info.pid != 2468 || info.session != "$3" || info.window != "@9" || info.socket != socket {
			t.Fatalf("literal metadata %+v, %v", info, err)
		}
	}
	for _, data := range []string{"", "%17|2468|0|$3|@9|/fixture/socket", "%17|2468|0|$3|@9", "%18|2468|0|$3|@9|/s\n", "%17|2469|0|$3|@9|/s\n", "%17|2468|1|$3|@9|/s\n", "%17|02468|0|$3|@9|/s\n", "%17|0|0|$3|@9|/s\n", "%17|2468|false|$3|@9|/s\n", "%17|2468|0|session|@9|/s\n", "%17|2468|0|$3|window|/s\n", "%17|2468|0|$3|@9|relative\n", "%17|2468|0|$3|@9|/s\x00\n", "%17|2468|0|$3|@9|/" + strings.Repeat("s", 4096) + "\n", strings.Repeat("x", 65537)} {
		if _, err := parseAttachInfo([]byte(data), "%17", 2468); err == nil {
			t.Fatal("malformed or changed metadata accepted")
		}
	}
	for _, token := range []string{"%0", "%17", "%18446744073709551615"} {
		if !attachToken(token, '%') {
			t.Fatal("valid pane token rejected")
		}
	}
	for _, token := range []string{"%", "%01", "%-1", "%+1", "%18446744073709551616", "%1\n", "%1\x00", "%１", "$1"} {
		if attachToken(token, '%') {
			t.Fatal("noncanonical pane token accepted")
		}
	}
}

func TestAttachContextLastCommas(t *testing.T) {
	for raw, want := range map[string]string{"/fixture/sock,12,0": "/fixture/sock", "/fixture/,with,commas ,123,7": "/fixture/,with,commas "} {
		got, err := attachContext(raw)
		if err != nil || got != want {
			t.Fatal("socket tuple not preserved", err)
		}
	}
	for _, raw := range []string{"", "/fixture/sock", "relative,12,0", "/fixture/sock,0,0", "/fixture/sock,01,0", "/fixture/sock,12,-1", "/fixture/sock,12,01", "/fixture/sock,12,", "/fixture/\x00sock,12,0"} {
		if _, err := attachContext(raw); err == nil {
			t.Fatal("malformed context accepted")
		}
	}
}
