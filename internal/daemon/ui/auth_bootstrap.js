// First executable script. Only the bounded capture and synchronous URL removal
// precede document ownership; modules/storage/API work start afterward.
(() => {
  let raw = location.hash;
  let capture = raw.length <= 8193 ? raw.slice(1) : null;
  raw = null;
  const failure = () => {
    const status = document.getElementById("nav-auth-status");
    if (status) status.textContent = "Link invalid, expired or already used; run aiq ui again.";
  };
  try { history.replaceState(history.state, "", location.pathname + location.search); }
  catch { capture = null; failure(); return; }
  let current = null;
  const active = owner => current === owner && owner.live;
  function retire(owner) {
    if (!owner?.live) return;
    owner.live = false; owner.fragment = null;
    removeEventListener("pagehide", owner.hide);
    try { owner.abort.abort(); } catch { /* Independent cleanup continues. */ }
    const session = owner.session; owner.session = null;
    try { session?.dispose(); } catch { /* No secret-bearing error rendering. */ }
  }
  function makeOwner(fragment) {
    const owner = {live:true, fragment, abort:new AbortController(), session:null, hide:null};
    owner.hide = () => retire(owner);
    addEventListener("pagehide", owner.hide, {once:true});
    return owner;
  }
  function load(owner) {
    if (!active(owner)) return;
    const build = document.documentElement.dataset.aiqBuild;
    if (typeof build !== "string" || !/^[A-Za-z0-9._+-]{1,128}$/.test(build)) { failure(); retire(owner); return; }
    if (document.documentElement.dataset.aiqUnavailable === "1") {
      const status=document.getElementById("nav-auth-status");
      if(status)status.textContent="Navigator unavailable; check the daemon and run aiq ui again.";
      retire(owner);return;
    }
    import("/assets/" + build + "/auth_session.js").then(module => {
      if (!active(owner)) return;
      let fragment = owner.fragment; owner.fragment = null;
      let session;
      try { session = module.start({window, document, build, fragment, signal:owner.abort.signal, isCurrent:() => active(owner)}); }
      finally { fragment = null; }
      if (active(owner)) owner.session = session;
      else session?.dispose();
    }).catch(() => { if (active(owner)) { failure(); retire(owner); } });
  }
  try {
    current = makeOwner(capture); capture = null;
    addEventListener("pageshow", event => {
      if (!event.persisted) return;
      try {
        retire(current);
        current = makeOwner(""); // Restoration has no old fragment authority.
        load(current);
      } catch { retire(current); failure(); }
    });
    load(current);
  } catch { capture = null; retire(current); failure(); }
})();
