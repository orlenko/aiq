const refused = () => { throw new TypeError("Navigator request unavailable"); };
const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
export function canonical32(value) {
  return typeof value === "string" && /^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/.test(value);
}
export function encodeForm(value) {
  return encodeURIComponent(value).replace(/[!'()*]/g, c => "%" + c.charCodeAt(0).toString(16).toUpperCase()).replace(/%20/g,"+");
}
function decodeForm(raw) {
  const value = decodeURIComponent(raw.replace(/\+/g," "));
  if (encodeForm(value) !== raw) refused();
  return value;
}
function pairs(raw, allowed) {
  const out = Object.create(null);
  if (raw === "") return out;
  for (const part of raw.split("&")) {
    const at = part.indexOf("=");
    if (at < 1) refused();
    const key = decodeForm(part.slice(0,at)), value = decodeForm(part.slice(at+1));
    if (!allowed.includes(key) || Object.hasOwn(out,key)) refused();
    out[key] = value;
  }
  if (Object.keys(out).sort().map(k => encodeForm(k)+"="+encodeForm(out[k])).join("&") !== raw) refused();
  return out;
}
export function directory(value) {
  if (typeof value !== "string" || value === "" || !value.startsWith("/") || /[\\\u0000-\u001f\u007f]/.test(value) || new TextEncoder().encode(value).length > 4096) refused();
  // A canonical UI route never silently normalizes a directory.
  if (value !== "/" && (value.endsWith("/") || value.split("/").slice(1).some(p => p === "" || p === "." || p === ".."))) refused();
  encodeURIComponent(value); // Refuse lone UTF-16 surrogates.
  return value;
}
export function route(value = "/sessions") {
  if (typeof value !== "string" || value.length > 8192 || /[\\#\u0000-\u001f\u007f]/.test(value)) refused();
  const split = value.indexOf("?"), path = split < 0 ? value : value.slice(0,split), raw = split < 0 ? "" : value.slice(split+1);
  if (split >= 0 && raw === "") refused();
  const parts = path.split("/");
  const detail = parts.length === 4 && parts[1] === "sessions" && ["claude","codex","agy"].includes(parts[2]) && uuid.test(parts[3]);
  if (!["/sessions","/asks","/","/help"].includes(path) && !detail) refused();
  const query = pairs(raw,["all","dir","at"]);
  if (Object.hasOwn(query,"dir")) directory(query.dir);
  if (Object.hasOwn(query,"all") && (path !== "/asks" || !["true","false"].includes(query.all))) refused();
  if (query.all === "true" && Object.hasOwn(query,"dir")) refused();
  if (Object.hasOwn(query,"at") && (!detail || !/^[1-9][0-9]{0,12}$/.test(query.at))) refused();
  return Object.freeze({path, dir:query.dir ?? "", all:query.all === "true", anchor:Number(query.at ?? 0), next:value});
}
export function fragment(raw) {
  if (typeof raw !== "string" || raw.length > 8192) refused();
  if (raw === "") return {code:null, next:route()};
  const fields = pairs(raw,["code","next"]);
  if (!canonical32(fields.code)) refused();
  return {code:fields.code, next:route(fields.next)};
}
export function origin(value) {
  if (typeof value !== "string" || !/^http:\/\/127\.0\.0\.1:[1-9][0-9]{0,4}$/.test(value) || Number(value.slice(value.lastIndexOf(":")+1)) > 65535) refused();
  return value;
}
export function credential(raw) {
  const text = new TextDecoder("utf-8",{fatal:true,ignoreBOM:true}).decode(raw);
  const match = /^[ \t\r\n]*\{[ \t\r\n]*"credential"[ \t\r\n]*:[ \t\r\n]*"([A-Za-z0-9_-]{43})"[ \t\r\n]*\}[ \t\r\n]*$/.exec(text);
  if (!match || !canonical32(match[1])) refused();
  return match[1];
}
