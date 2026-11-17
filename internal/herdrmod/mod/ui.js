import { div } from "gpui-kit";
import { Button, TextView, Input } from "gpui-base";
import { theme } from "herdr";
import { documentHTML } from "./markdown.js";
export const appearances = ["Herdr", "Light", "Dark"];
const appearanceKey = "aiq.appearance";

// The app supplies these colors only to elements inside its own pane.
const palettes = {
  Light: {
    surface: "#f9fafc", chrome: "#eef2f7", raised: "#e1eafb", border: "#c5cedc",
    foreground: "#202b3b", muted: "#526178", dim: "#69788e", accent: "#3563a8",
    ansi1: "#b64738", ansi2: "#2c8059", ansi3: "#a86c20",
  },
  Dark: {
    surface: "#18202c", chrome: "#131a25", raised: "#243653", border: "#37465b",
    foreground: "#e6edf8", muted: "#a9b8cd", dim: "#8797ad", accent: "#8fb9f3",
    ansi1: "#ee8678", ansi2: "#6bc89f", ansi3: "#e6b266",
  },
};

export function loadAppearance() {
  try {
    const saved = globalThis.localStorage?.getItem(appearanceKey);
    if (appearances.includes(saved)) return saved;
  } catch {
    // The pane can still use Herdr's appearance if storage is unavailable.
  }
  return "Herdr";
}

export function saveAppearance(value) {
  try {
    globalThis.localStorage?.setItem(appearanceKey, value);
  } catch {
    // A storage failure only makes this change last until the pane reloads.
  }
}

export function createUI(getAppearance) {
  const colors = () => {
    const appearance = getAppearance();
    if (appearance !== "Herdr") return palettes[appearance];
    const inherited = theme();
    return {...inherited, chrome: inherited.surface};
  };
  return {
    colors,
    label(text, muted = false) {
      const t = colors();
      return div().text_size(13).text_color(muted ? t.muted : t.foreground).child(String(text ?? ""));
    },
    button(id, text, click, active = false, disabled = false) {
      const t = colors();
      return Button.new(id).px(12).py(7).rounded(5).border(1)
        .border_color(active ? t.accent : t.border).bg(active ? t.raised : t.surface)
        .text_color(disabled ? t.dim : t.foreground).text_size(13).disabled(disabled)
        .on_click(click).child(text);
    },
    input(state) {
      const t = colors();
      return Input.new(state).bg(t.surface).text_color(t.foreground).border(1).border_color(t.border).p(8);
    },
    panel(id) {
      return div().id(id).min_w(0).min_h(0).flex_1().overflow_y_scrollbar().p(24)
        .text_color(colors().foreground);
    },
  };
}

export function makeProse(colors) {
  const cache=new Map();let bytes=0;
  const prose=(id,text,marks=false)=>{
    const key=id+":"+marks;let entry=cache.get(key);
    if(!entry || entry.text!==text){
      if(entry){bytes-=entry.bytes;cache.delete(key);}
      const html=documentHTML(text,marks);entry={text,html,bytes:2*(text.length+html.length)};
      while(cache.size && (cache.size>=32||bytes+entry.bytes>8*1024*1024)){const key=cache.keys().next().value;bytes-=cache.get(key).bytes;cache.delete(key);}
      if(entry.bytes<=8*1024*1024){cache.set(key,entry);bytes+=entry.bytes;}
    }
    return TextView.html(id,entry.html).text_color(colors().foreground)
      .selectable(true).scrollable(false).on_link_click(()=>{});
  };
  prose.clear=()=>{cache.clear();bytes=0;};return prose;
}
