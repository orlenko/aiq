package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStatusPageCodexAuthenticationRemedy(t *testing.T) {
	_, script, ok := strings.Cut(string(uiHTML), "<script>\n")
	if !ok {
		t.Fatal("status script absent")
	}
	helpers, _, ok := strings.Cut(script, "// refresh:begin\n")
	if !ok {
		t.Fatal("status helpers absent")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for status-page regression tests")
		}
		t.Skip("node not installed; status-page regression NOT RUN")
	}
	fixture := `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = ` + strconv.Quote(helpers) + `;
function card(changes = {}) {
  const account = {id:'codex/demo',provider:'codex',enabled:true,has_credential:true,
    eligible:false,ineligible:'authentication required',windows:[],leases:[],score:0,rank:0,...changes};
  return vm.runInNewContext(source + '\naccountCard(account)', {account});
}
const remedy = /Run <code>(.*?)<\/code> in a terminal\./;
for (const changes of [{}, {exhausted:true}, {poll_error:'invented rejected token'},
  {windows:[{key:'weekly',label:'Weekly',used_pct:83,binding:true,observed_at:1}]}]) {
  const html=card(changes);
  assert.equal(remedy.exec(html)?.[1],'aiq account login codex/demo');
  assert.ok(html.includes('authentication required'));
}
for (const changes of [{eligible:true},{enabled:false},{provider:'claude',id:'claude/demo'},
  {provider:'agy',id:'agy/demo'}, ...['','network timeout','rate limited','provider unavailable',
  'quota exhausted','raw 401 Unauthorized'].map(ineligible=>({ineligible}))]) {
  assert.equal(remedy.test(card(changes)),false,JSON.stringify(changes));
}
for (const changes of [{has_credential:false},{cli_login:'out'},{cli_mismatch:true,cli_identity:'demo@example.invalid'}]) {
  assert.ok(card(changes).includes('aiq account login codex/demo'),'existing login guidance retained');
}
for (const id of ['codex/demo;literal','codex/demo$(literal)','codex/demo'+String.fromCharCode(96)+'literal'+String.fromCharCode(96),"codex/demo'quote",'codex/<demo>&']) {
  const command=remedy.exec(card({id}))?.[1];
  assert.ok(command,'login remedy missing for invented account identifier');
  // Compare the HTML-decoded literal shell argument spelling. Execute nothing.
  const text=command.replace(/&quot;/g,'"').replace(/&gt;/g,'>').replace(/&lt;/g,'<').replace(/&amp;/g,'&');
  const expected="aiq account login '"+id.replace(/'/g,"'\\''")+"'";
  assert.equal(text,expected);
  assert.ok(!command.includes('<demo>'),'identifier escaped for HTML');
}
console.log('authentication remedy and refusal/quoting classes passed');
`
	path := filepath.Join(t.TempDir(), "login-remedy.js")
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("status-page fixture: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "authentication remedy and refusal/quoting classes passed" {
		t.Fatalf("fixture did not finish: %s", out)
	}
}
