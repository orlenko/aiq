import { div } from "gpui-kit";
import { Button, TextView } from "gpui-base";
import { theme } from "herdr";
import { documentHTML } from "./markdown.js";
export function label(text, muted=false) { const t=theme(); return div().text_size(13).text_color(muted?t.muted:t.foreground).child(String(text??"")); }
export function button(id,text,click,active=false,disabled=false) {
  const t=theme();return Button.new(id).px(12).py(7).rounded(5).border(1).border_color(active?t.accent:t.border).bg(active?t.raised:t.surface).text_color(disabled?t.dim:t.foreground).text_size(13).disabled(disabled).on_click(click).child(text);
}
export function makeProse() {
  const cache=new Map();let bytes=0;
  const prose=(id,text,marks=false)=>{
    const key=id+":"+marks;let entry=cache.get(key);
    if(!entry || entry.text!==text){
      if(entry){bytes-=entry.bytes;cache.delete(key);}
      const html=documentHTML(text,marks);entry={text,html,bytes:2*(text.length+html.length)};
      while(cache.size && (cache.size>=32||bytes+entry.bytes>8*1024*1024)){const key=cache.keys().next().value;bytes-=cache.get(key).bytes;cache.delete(key);}
      if(entry.bytes<=8*1024*1024){cache.set(key,entry);bytes+=entry.bytes;}
    }
    return TextView.html(id,entry.html).selectable(true).scrollable(false).on_link_click(()=>{});
  };
  prose.clear=()=>{cache.clear();bytes=0;};return prose;
}
export function panel(id) { const t=theme(); return div().id(id).min_w(0).min_h(0).flex_1().overflow_y_scrollbar().p(24).text_color(t.foreground); }
