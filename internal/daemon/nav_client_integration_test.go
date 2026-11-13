//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/orlenko/aiq/internal/navclient"
	"github.com/orlenko/aiq/internal/navtoken"
)

// Only invented lifecycle fixtures and their private temporary state are used.
// This proves the public client and actual daemon adapter agree, including the
// proof parser, exact application bytes, one-use exchange and revoke semantics.
func TestNavigatorClientRealAdapter(t *testing.T) {
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("invented")) }))
	g, _ := lifeStart(t, s)
	done := make(chan struct{})
	go func() { s.serveNavigator(g); close(done) }()
	t.Cleanup(func() { _ = g.Close(); lifeWait(t, done) })
	record, err := navtoken.Read(navTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	c, err := navclient.New(record, navBuild())
	if err != nil {
		t.Fatal(err)
	}
	i, err := c.Info(context.Background())
	if err != nil || i.Root != "" || i.Hostname != "invented-host" || i.DataDir != g.handle.scope.dataDir {
		t.Fatal("real adapter info did not authenticate")
	}
	link, err := c.Issue(context.Background())
	value, ok := link.Value()
	if err != nil || !ok {
		t.Fatal("real adapter issue failed")
	}
	body := `{"action":"exchange","code":"` + value + `"}`
	status, _, raw := lifeHTTP(t, g, "POST", "/api/nav/login", body, "", nil, "")
	var credential struct {
		Credential string `json:"credential"`
	}
	if status != 200 || json.Unmarshal(raw, &credential) != nil || !g.auth.Credential(credential.Credential) {
		t.Fatal("issued link did not exchange")
	}
	status, _, _ = lifeHTTP(t, g, "POST", "/api/nav/login", body, "", nil, "")
	if status != 401 {
		t.Fatal("link was not one-use")
	}
	if err := c.Revoke(context.Background()); err != nil || g.auth.Credential(credential.Credential) {
		t.Fatal("real adapter revoke failed")
	}
	_ = g.Close()
	if info, err := c.Info(context.Background()); err != navclient.ErrUnavailable || info != (navclient.Info{}) {
		t.Fatal("retired listener yielded metadata")
	}
}
