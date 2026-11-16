package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadFleet(t *testing.T) {
	writeConfig(t, "version = 2\n[fleet]\nhosts = [\"invented-box\", \"me@invented.lan\", \"invented_2\"]\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Fleet.Hosts) != 3 || c.Fleet.AiqPath() != DefaultFleetAiq {
		t.Fatalf("%+v", c.Fleet)
	}
}

// A host or path that ssh could read as an option, or a remote shell as
// more than one word, never reaches ssh.
func TestLoadRefusesUnsafeFleetValues(t *testing.T) {
	for _, body := range []string{
		`hosts = ["-oProxyCommand=touch /tmp/x"]`,
		`hosts = ["invented box"]`,
		`hosts = ["invented;id"]`,
		`hosts = ["invented$(id)"]`,
		`hosts = ["invented-box", "invented-box"]`,
		`hosts = [""]`,
		`hosts = ["invented-box:22"]`,
		`aiq = "-x"`,
		`aiq = "aiq; id"`,
		`aiq = "$HOME/aiq"`,
		"aiq = \"a\\nb\"",
	} {
		writeConfig(t, "version = 2\n[fleet]\n"+body+"\n")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "fleet.") {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// Defaults are not written back: saving a config without [fleet] adds none.
func TestSaveWritesNoFleetNobodySet(t *testing.T) {
	writeConfig(t, "version = 2\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(os.Getenv("AIQ_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fleet") {
		t.Fatalf("saved:\n%s", b)
	}
}
