export let isChecking=true;
export function setChecking(value){isChecking=value;}
export const checking=()=>isChecking;
export const machines=()=>[{name:null,layout:'ready'}];
export const panes=()=>[];
export const focused=()=>({tab:null});
export const nextCommand=()=>new Promise(()=>{});
export const exec=()=>{throw new Error('unexpected native exec');};
export const hc=exec,run=exec;
export const theme=()=>new Proxy({},{get:()=>'#888888'});
export class View {}
function element(){const state={handlers:{}};return new Proxy(state,{get:(s,key)=>key==='handlers'?s.handlers:(...args)=>{if(key==='on_key_down')s.handlers.key=args[0];return elementWith(s);}});}
function elementWith(state){return new Proxy(state,{get:(s,key)=>key==='handlers'?s.handlers:(...args)=>{if(key==='on_key_down')s.handlers.key=args[0];return elementWith(s);}});}
export const div=element,h_flex=element,v_flex=element;
export const Button={new:element},Table=Button,TableHeader=Button,TableBody=Button,TableRow=Button,TableHead=Button,TableCell=Button,Input=Button;
export const TextView={html:element};
export const ScrollHandle={new:()=>({last:null,scroll_to_item(i){this.last=i;}})};
export const InputState={new:()=>({text:'',on(){},value(){return this.text;},set_value(text){this.text=text;}})};
export function context(){const tasks=[];return {tasks,notify(){},stop_propagation(){},focus_handle(){return {focus(){}};},timer:{every(){}},spawn(fn){const p=fn(this);tasks.push(p);return p;},async drain(){while(tasks.length)await tasks.shift();}};}
