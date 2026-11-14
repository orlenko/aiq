const channels = Object.freeze({directories:["catalog"],sessions:["dir","workers","all"], conversation:["provider","id","last","markup","anchor","etag","fromTurn","fromKey"], state:["provider","id","selection"], asks:["mode","dir"], placement:["askId"]});
const unavailable = () => new TypeError("Navigator request unavailable");
function queryCopy(kind, query) {
  if (!query || typeof query !== "object" || Array.isArray(query)) throw unavailable();
  const copy = {};
  for (const key of channels[kind]) {
    const field = Object.getOwnPropertyDescriptor(query,key);
    // Asks all-scope has no directory. The fixed reader validates semantics.
    if (!field && kind === "asks" && key === "dir") continue;
    if (!field && kind === "conversation" && ["etag","fromTurn","fromKey"].includes(key)) continue;
    if(!field&&(kind==="directories"&&key==="catalog"||kind==="sessions"&&key==="all"))continue;
    if (!field || !Object.hasOwn(field,"value") || !["string","number","boolean"].includes(typeof field.value)) throw unavailable();
    copy[key] = field.value;
  }
  return Object.freeze(copy);
}

// One physical reader for this auth owner. Each current route has a bounded
// work descriptor containing the latest intent of each fixed channel. Readers
// must settle only after their physical fetch/body cleanup is complete.
export function createReadBroker(readers) {
  const methods = Object.create(null);
  for (const kind of Object.keys(channels)) {
    const field = Object.getOwnPropertyDescriptor(readers,kind);
    if (field) {
      if (!Object.hasOwn(field,"value") || typeof field.value !== "function") throw unavailable();
      methods[kind] = field.value;
    }
  }
  let closed = false, current = null, active = null, queued = false, serial = 0n;
  function owns(route) { return !closed && current === route && !route.retired; }
  function unlink(job) {
    try { job.parent?.removeEventListener("abort",job.onAbort); } catch { /* Retirement stays local. */ }
  }
  function settle(job, ok, value) {
    if (job.settled) { unlink(job); return; }
    job.settled = true;
    if (job.route.jobs.get(job.kind) === job) job.route.jobs.delete(job.kind);
    unlink(job);
    const resolve = job.resolve, reject = job.reject;
    job.resolve = job.reject = null;
    if (ok && !job.cancelled && owns(job.route)) resolve(value);
    else reject(unavailable());
    job.query = null;
  }
  function abort(job) {
    if (!job || job.settled) return;
    job.cancelled = true;
    if (active === job) {
      // The logical promise also waits for physical cleanup; abort alone does
      // not free either the upstream controller's flight or this slot.
      try { job.controller?.abort(); } catch { /* Completion still owns release. */ }
    } else settle(job,false);
  }
  function retire(route) {
    if (!route || route.retired) return;
    route.retired = true;
    const jobs = [...route.jobs.values()];route.jobs.clear();
    for (const job of jobs) job.cancelled = true;
    for (const job of jobs) if (job !== active) settle(job,false);
    if (active?.route === route) abort(active);
  }
  function finish(job, ok, value) {
    if (active !== job) return;
    active = null;
    settle(job,ok,value);
    pump();
  }
  function start(job) {
    active = job;
    try {
      job.controller = new AbortController();
      if (!owns(job.route) || job.cancelled || job.parent?.aborted) { finish(job,false); return; }
      const result = methods[job.kind](job.query,job.controller.signal);
      Promise.resolve(result).then(value=>finish(job,true,value),()=>finish(job,false));
    } catch { finish(job,false); }
  }
  function pump() {
    if (closed || queued || active || !current || current.retired) return;
    queued = true;
    queueMicrotask(()=>{
      queued = false;
      if (closed || active || !current || current.retired) return;
      let next = null;
      for (const job of current.jobs.values()) {
        if (!job.settled && !job.cancelled && (!next || job.order < next.order)) next = job;
      }
      if (next) start(next);
    });
  }
  function route() {
    const previous = current, work = {jobs:new Map(),retired:false};
    if (!closed) current = work;
    else work.retired = true;
    retire(previous);
    return Object.freeze({
      read(kind, query, signal) {
        if (!owns(work) || !Object.hasOwn(methods,kind)) return Promise.reject(unavailable());
        let copy;
        try { copy = queryCopy(kind,query); } catch { return Promise.reject(unavailable()); }
        if (!owns(work)) return Promise.reject(unavailable());
        return new Promise((resolve,reject)=>{
          const old = work.jobs.get(kind);
          const job = {route:work,kind,query:copy,parent:signal,order:++serial,resolve,reject,
            settled:false,cancelled:false,controller:null,onAbort:null};
          job.onAbort = ()=>{abort(job);pump();};
          work.jobs.set(kind,job);
          abort(old);
          try {
            if (signal?.aborted) abort(job);
            else signal?.addEventListener("abort",job.onAbort,{once:true});
          } catch { abort(job); }
          if (!owns(work) || work.jobs.get(kind) !== job || job.settled) { abort(job);unlink(job); }
          pump();
        });
      },
      dispose() {
        if (current === work) current = null;
        retire(work);pump();
      }
    });
  }
  function close() {
    if (closed) return;
    closed = true;
    const old = current;current = null;retire(old);
  }
  return Object.freeze({route,close});
}
