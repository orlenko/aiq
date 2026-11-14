package config

import "testing"

func TestUILauncherLegacyAndReservation(t *testing.T) {
	if ValidLauncherName("ui") == nil {
		t.Fatal("new ui launcher admitted")
	}
	writeConfig(t, "[launchers.ui]\nprovider='claude'\ncommand='/invented/never-run'\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Launcher("ui"); !ok {
		t.Fatal("legacy launcher discarded")
	}
	delete(c.Launchers, "ui")
	if err = Save(c); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Launcher("ui"); ok {
		t.Fatal("legacy removal failed")
	}
}
