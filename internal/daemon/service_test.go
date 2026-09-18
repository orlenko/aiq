package daemon

import "testing"

func TestMergePATHKeepsOrderAndAddsLoginEntries(t *testing.T) {
	got := mergePATH("/usr/bin:/home/u/.local/bin", "/home/u/.local/share/aiq/shims:/home/u/.nvm/bin:/usr/bin")
	want := "/usr/bin:/home/u/.local/bin:/home/u/.local/share/aiq/shims:/home/u/.nvm/bin"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if mergePATH("/a", "") != "/a" {
		t.Fatal("an empty login PATH must leave the current one")
	}
}
