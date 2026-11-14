import {canonical32} from "./auth_codec.js";
const encoder=new TextEncoder();
const bad=()=>{throw new TypeError("Navigator request unavailable");};
const alphabet="ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
function encode(bytes) {
 let out="",bits=0,value=0;
 for(const b of bytes){value=(value<<8)|b;bits+=8;while(bits>=6){bits-=6;out+=alphabet[(value>>>bits)&63];}}
 if(bits)out+=alphabet[(value<<(6-bits))&63];return out;
}
function decode(value) {
 if(!canonical32(value))bad();const out=new Uint8Array(32);let bits=0,n=0,word=0;
 for(const c of value){word=(word<<6)|alphabet.indexOf(c);bits+=6;if(bits>=8){bits-=8;out[n++]=(word>>>bits)&255;}}
 return out;
}
function frame(...fields) {
 const parts=fields.map(s=>encoder.encode(s));let size=0;
 for(const p of parts)size+=String(p.length).length+1+p.length;
 const out=new Uint8Array(size);let at=0;
 for(const p of parts){const prefix=encoder.encode(String(p.length)+":");out.set(prefix,at);at+=prefix.length;out.set(p,at);at+=p.length;}
 return out;
}
// No global key or credential. The caller owns this operation through every
// WebCrypto await; refusal/disposal must retain its physical slot until settled.
export async function prepareTabRead(credential,base,build,path,valid,crypto=globalThis.crypto) {
 return prepareTabProof(credential,base,build,path,"",valid,crypto,false);
}
export async function prepareTabRead2(credential,base,build,path,condition,valid,crypto=globalThis.crypto){
 if(typeof condition!=="string"||condition!==""&&!/^"[0-9a-f]{64}"$/.test(condition))bad();
 return prepareTabProof(credential,base,build,path,condition,valid,crypto,true);
}
async function prepareTabProof(credential,base,build,path,condition,valid,crypto,conditional){
 const check=()=>{if(!valid())bad();};check();
 const digest=await crypto.subtle.digest("SHA-256",decode(credential));credential=null;check();
 const key=await crypto.subtle.importKey("raw",digest,{name:"HMAC",hash:"SHA-256"},false,["sign","verify"]);check();
 const selector=encode(new Uint8Array(await crypto.subtle.sign("HMAC",key,encoder.encode("aiq-nav-tab-id-v1"))));check();
 const bytes=new Uint8Array(32);crypto.getRandomValues(bytes);check();const nonce=encode(bytes);
 const fields=[conditional?"aiq-nav-tab-request-v2":"aiq-nav-tab-request-v1",base,build,"GET",path,selector,nonce];if(conditional)fields.push(condition);
 const requestMAC=encode(new Uint8Array(await crypto.subtle.sign("HMAC",key,frame(...fields))));check();
 return Object.freeze({authorization:(conditional?"AiqTab2 ":"AiqTab ")+selector+"."+nonce+"."+requestMAC,
  async verify(proof,status,mime,responseBuild,raw,etag=""){
   check();if(!canonical32(proof)||mime!==(conditional&&status===304?"":"application/json")||responseBuild!==build||!(raw instanceof Uint8Array))bad();
   if(conditional&&(etag!==""&&!/^"[0-9a-f]{64}"$/.test(etag)||status===304&&(condition===""||etag!==condition||raw.byteLength!==0)))bad();
   const hash=encode(new Uint8Array(await crypto.subtle.digest("SHA-256",raw)));check();
   const responseFields=[conditional?"aiq-nav-tab-response-v2":"aiq-nav-tab-response-v1",requestMAC,String(status),mime,build];if(conditional)responseFields.push(etag);responseFields.push(hash);
   const ok=await crypto.subtle.verify("HMAC",key,decode(proof),frame(...responseFields));check();if(!ok)bad();
  }});
}
