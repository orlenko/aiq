// Independent invented-fixture signer; never embedded in production assets.
import assert from 'node:assert/strict';
import {createHash,createHmac} from 'node:crypto';
const bodies=new WeakMap();
const enc=b=>Buffer.from(b).toString('base64url');
const frame=(...fields)=>Buffer.concat(fields.flatMap(s=>{const b=Buffer.from(s);return[Buffer.from(String(b.length)+':'),b];}));
export function rememberBody(response,body){if(typeof body==='string'||body instanceof Uint8Array)bodies.set(response,Buffer.from(body));return response;}
export function signReply(response,options,credential,build){
 if(options.method!=='GET'||!bodies.has(response))return response;
 const parts=options.headers.Authorization?.match(/^AiqTab ([\w-]{43})\.([\w-]{43})\.([\w-]{43})$/);assert(parts,'data request must not send raw Bearer');
 const key=createHash('sha256').update(Buffer.from(credential,'base64url')).digest();
 const mac=enc(createHmac('sha256',key).update(frame('aiq-nav-tab-response-v1',parts[3],String(response.status),'application/json',build,enc(createHash('sha256').update(bodies.get(response)).digest()))).digest());
 response.headers.set('X-Aiq-Tab-Proof',mac);return response;
}
export async function until(predicate){const end=Date.now()+3000;while(!predicate()){if(Date.now()>end)throw Error('invented fetch did not start');await new Promise(r=>setImmediate(r));}}
