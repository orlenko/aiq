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

// Execute the page's real helpers, flag and refresh body once per simulated page.
// All responses and DOM nodes are invented; the fixture never opens a socket.
func TestStatusPageRefreshHandlesBusyResponses(t *testing.T) {
	page := string(uiHTML)
	_, script, ok := strings.Cut(page, "<script>\n")
	if !ok {
		t.Fatal("ui.html has no script")
	}
	script, _, ok = strings.Cut(script, "</script>")
	if !ok {
		t.Fatal("ui.html has no script end")
	}
	helpers, refresh, ok := strings.Cut(script, "// refresh:begin\n")
	if !ok {
		t.Fatal("ui.html has no refresh beginning marker")
	}
	refresh, scheduler, ok := strings.Cut(refresh, "// refresh:end\n")
	if !ok {
		t.Fatal("ui.html has no refresh end marker")
	}
	// Bind the surrounding scheduler to the existing initial request, interval and
	// manual-poll delay, rather than testing a duplicate scheduler in the fixture.
	const wantScheduler = `document.getElementById('poll').onclick = async () => { await fetch('/api/poll', { method: 'POST' }); setTimeout(refresh, 3000); };
refresh();
setInterval(refresh, 5000);`
	if strings.TrimSpace(scheduler) != wantScheduler {
		t.Fatal("the status-page refresh scheduler changed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, and CI is set")
		}
		t.Skip("node is not installed")
	}
	fixture := `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = ` + strconv.Quote(helpers+refresh) + `;
const scheduler = ` + strconv.Quote(scheduler) + `;

function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}
function response(status, data, error) {
  return { status, ok: status === 200, jsonCalls: 0,
    async json() {
      this.jsonCalls++;
      if (status === 503) throw new Error('503 JSON must never be read');
      if (error) throw error;
      return data;
    },
  };
}
function state(name, port, empty = false) {
  return {
    daemon: { navigator_port: port, last_poll: 0, poll_interval: 300 },
    view: { hostname: name, model_scope: {}, rankings: {}, accounts: empty ? [] : [{
      id: 'codex/' + name, provider: 'codex', enabled: true, has_credential: true,
      eligible: true, rank: 1, score: 2, windows: [], leases: [],
    }] },
  };
}
function page() {
  let html = '', writes = 0, failCommit = false;
  function element(tag) {
    return {
      tag, children: [], textContent: '', attrs: {},
      set innerHTML(_) { throw new Error('unexpected HTML sink: ' + tag); },
      setAttribute(k, v) { this.attrs[k] = String(v); },
      appendChild(c) { this.children.push(c); },
      replaceChildren() { this.children = []; },
    };
  }
  const nodes = { meta: element('span'), navlink: element('span'), main: {
    get innerHTML() { return html; },
    set innerHTML(value) {
      if (failCommit) { failCommit = false; throw new Error('fixture main commit failed'); }
      html = value;
      writes++;
    },
  } };
  const queue = [], calls = [];
  const context = vm.createContext({
    document: {
      getElementById(id) { assert.ok(id in nodes, id); return nodes[id]; },
      createElement: element,
    },
    fetch(url, options) {
      calls.push(url);
      assert.equal(url, '/api/state');
      assert.equal(JSON.stringify(options), '{"cache":"no-store"}');
      assert.ok(queue.length, 'unexpected extra request');
      const next = queue.shift();
      return next instanceof Error ? Promise.reject(next) : Promise.resolve(next);
    },
    setTimeout() { assert.fail('refresh scheduled an extra timeout'); },
    setInterval() { assert.fail('refresh scheduled an extra interval'); },
  });
  vm.runInContext(source, context);
  assert.equal(calls.length, 0, 'extracting helpers must not poll');
  return {
    nodes,
    snapshot() { return { html, writes }; },
    failCommit() { failCommit = true; },
    start(next) {
      const count = calls.length;
      queue.push(next);
      const pending = vm.runInContext('refresh()', context);
      assert.equal(calls.length, count + 1);
      return pending;
    },
    async step(next) {
      const count = calls.length;
      await this.start(next);
      assert.equal(calls.length, count + 1, 'one refresh must make exactly one request');
      assert.equal(queue.length, 0);
      if (!(next instanceof Error)) assert.equal(next.jsonCalls, next.status === 503 ? 0 : 1);
    },
  };
}
function link(p, port) {
  const children = p.nodes.navlink.children;
  assert.equal(children.length, port === null ? 0 : 1);
  if (port !== null) {
    assert.equal(children[0].href, 'http://127.0.0.1:' + port + '/');
    assert.equal(children[0].textContent, 'Sessions');
  }
}
function busy(p, previous, before) {
  assert.equal(p.nodes.meta.textContent,
    'daemon busy · retrying…' + (previous ? ' · showing previous state' : ''));
  assert.deepEqual(p.snapshot(), before, 'busy response must not write main');
  link(p, null);
}
function normal(p, name, port, empty = false) {
  assert.ok(p.nodes.meta.textContent.startsWith(name + ' · polled never every 300s'));
  assert.ok(!/busy|showing previous state|unreachable/.test(p.nodes.meta.textContent));
  assert.ok(p.snapshot().html.includes(empty ? 'no accounts' : 'codex/' + name));
  link(p, port);
}
function failed(p, before, message) {
  assert.ok(p.nodes.meta.textContent.startsWith('daemon unreachable: '));
  assert.ok(p.nodes.meta.textContent.includes(message));
  assert.deepEqual(p.snapshot(), before);
  link(p, null);
}

async function run() {
  // Initial and repeated 503 must not invent a previously displayed state.
  {
    const p = page(), before = p.snapshot();
    for (let i = 0; i < 2; i++) {
      await p.step(response(503));
      busy(p, false, before);
    }
  }
  // A completed empty view counts too. Distinct recovery content and ports must win.
  for (const empty of [true, false]) {
    const p = page();
    await p.step(response(200, state('fixture-first', 50001, empty)));
    normal(p, 'fixture-first', 50001, empty);
    const before = p.snapshot();
    for (let i = 0; i < 2; i++) {
      await p.step(response(503));
      busy(p, true, before);
    }
    await p.step(response(200, state('fixture-recovered', 50002)));
    normal(p, 'fixture-recovered', 50002);
    assert.notEqual(p.snapshot().html, before.html);
  }
  // Neither decoding, card building nor an unsuccessful main assignment is a display.
  for (const failure of ['json', 'card', 'commit', 'other-status']) {
    const p = page(), before = p.snapshot();
    const data = state('fixture-broken', 50001);
    if (failure === 'card') data.view.accounts[0].windows = null;
    if (failure === 'commit') p.failCommit();
    await p.step(response(failure === 'other-status' ? 502 : 200, data,
      failure === 'json' || failure === 'other-status' ? new Error('fixture malformed JSON') : null));
    failed(p, before, '');
    await p.step(response(503));
    busy(p, false, before);
    await p.step(response(200, state('fixture-after-failure', 50002)));
    normal(p, 'fixture-after-failure', 50002);
  }
  // A new r.ok policy would change this existing non-503 JSON/render behavior.
  {
    const p = page();
    await p.step(response(500, state('fixture-other-status', 50003)));
    normal(p, 'fixture-other-status', 50003);
  }
  // Network failure clears the link and preserves both main and the historical flag.
  for (const prior of [false, true]) {
    const p = page();
    if (prior) {
      await p.step(response(200, state('fixture-before-network', 50001)));
      normal(p, 'fixture-before-network', 50001);
    }
    const before = p.snapshot();
    await p.step(new Error('fixture network failure'));
    failed(p, before, 'fixture network failure');
    await p.step(response(503));
    busy(p, prior, before);
    await p.step(response(200, state('fixture-after-network', 50002)));
    normal(p, 'fixture-after-network', 50002);
  }
  // Later parse/render failures must not reset the already-true flag either.
  for (const renderFailure of [false, true]) {
    const p = page();
    await p.step(response(200, state('fixture-before-error', 50001)));
    const before = p.snapshot(), data = state('fixture-bad-later', 50002);
    if (renderFailure) data.view.rankings = null;
    await p.step(response(200, data, renderFailure ? null : new Error('fixture bad later JSON')));
    failed(p, before, '');
    await p.step(response(503));
    busy(p, true, before);
  }
  // Characterize inherited completion ordering; no latest-request arbitration.
  {
    const p = page(), a = deferred(), refusal = response(503);
    const first = p.start(a.promise);
    await p.step(response(200, state('fixture-newer-success', 50002)));
    normal(p, 'fixture-newer-success', 50002);
    const before = p.snapshot();
    a.resolve(refusal);
    await first;
    assert.equal(refusal.jsonCalls, 0);
    busy(p, true, before); // Flag is read at completion, not at request start.
  }
  {
    const p = page(), a = deferred(), older = response(200, state('fixture-older-success', 50001));
    const first = p.start(a.promise), before = p.snapshot();
    await p.step(response(503));
    busy(p, false, before);
    a.resolve(older);
    await first;
    assert.equal(older.jsonCalls, 1);
    normal(p, 'fixture-older-success', 50001);
  }
  {
    const p = page(), decoded = deferred(), enteredJSON = deferred();
    const first = p.start({ status: 200, json() { enteredJSON.resolve(); return decoded.promise; } });
    await enteredJSON.promise;
    await p.step(response(200, state('fixture-newer-view', 50002)));
    normal(p, 'fixture-newer-view', 50002);
    decoded.resolve(state('fixture-older-view', 50001));
    await first;
    normal(p, 'fixture-older-view', 50001);
  }
  // Run the actual scheduling tail with controlled callbacks and a pending POST.
  {
    const poll = {}, post = deferred(), events = [];
    const refresh = () => events.push('refresh');
    vm.runInNewContext(scheduler, {
      document: { getElementById(id) { assert.equal(id, 'poll'); return poll; } }, refresh,
      fetch(url, options) {
        assert.equal(url, '/api/poll');
        assert.equal(JSON.stringify(options), '{"method":"POST"}');
        events.push('post');
        return post.promise;
      },
      setTimeout(fn, ms) { assert.equal(fn, refresh); events.push('timeout:' + ms); },
      setInterval(fn, ms) { assert.equal(fn, refresh); events.push('interval:' + ms); },
    });
    assert.deepEqual(events, ['refresh', 'interval:5000']);
    const clicked = poll.onclick();
    assert.deepEqual(events, ['refresh', 'interval:5000', 'post']);
    post.resolve();
    await clicked;
    assert.deepEqual(events, ['refresh', 'interval:5000', 'post', 'timeout:3000']);
  }
  console.log('refresh transitions, inherited overlap ordering and scheduler passed');
}
run().catch(error => { console.error(error); process.exitCode = 1; });
`
	path := filepath.Join(t.TempDir(), "refresh.js")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "refresh transitions, inherited overlap ordering and scheduler passed" {
		t.Fatalf("node did not complete every fixture case: %s", out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
