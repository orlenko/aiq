import { div } from "gpui-kit";
import * as gpui from "gpui-kit";
import { Button, TextView, Input, h_flex, v_flex, Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from "gpui-base";
import { theme } from "herdr";
import { documentBlocks } from "./markdown.js";
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

function countdownRing(fraction, colors) {
  const painter = globalThis.window;
  if (fraction === null || !gpui.PathBuilder || typeof painter?.paint_path !== "function") return null;

  const radius = 7;
  const center = 8;
  const ring = div().size(16).relative().flex_shrink_0().child(
    div().absolute().inset_0().rounded_full().border(2).border_color(colors.border)
  );
  if (fraction > 0) {
    const sweep = 2 * Math.PI * fraction;
    const quarter = Math.PI / 2;
    const point = angle => [center + radius * Math.sin(angle), center - radius * Math.cos(angle)];
    const path = gpui.PathBuilder.stroke(2).move_to(center, center - radius);
    // Herdr's endpoint arcs distort at half a circle and can disappear near a
    // full one. Draw one continuous stroke with curves of at most a quarter turn.
    for (let start = 0; start < sweep; start += quarter) {
      const end = Math.min(start + quarter, sweep);
      const tangent = (4 / 3) * radius * Math.tan((end - start) / 4);
      const [x0, y0] = point(start);
      const [x1, y1] = point(end);
      path.cubic_bezier_to(x1, y1,
        x0 + tangent * Math.cos(start), y0 + tangent * Math.sin(start),
        x1 - tangent * Math.cos(end), y1 - tangent * Math.sin(end));
    }
    if (fraction === 1) path.close();
    ring.child(painter.paint_path(path.build(), colors.accent).absolute().inset_0());
  }
  return ring;
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
    countdown(value, tooltip) {
      const row = h_flex().w(96).flex_shrink_0().items_center().justify_end().gap(6).tooltip(tooltip);
      if (!value) return row.child(this.label("—",true));
      const ring = countdownRing(value.fraction, colors());
      if (ring) row.child(ring);
      return row.child(this.label(value.text,true));
    },
    warning(text) {
      const t = colors();
      return div().min_w(0).border_l(3).border_color(t.ansi3).bg(t.chrome).px(10).py(5)
        .text_size(13).text_color(t.foreground).child(text);
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

function richText(id, html, color) {
  return TextView.html(id,html).w_full().min_w(0).text_color(color)
    .selectable(true).scrollable(false).on_link_click(()=>{});
}

function proseTable(id, block, colors) {
  const { table, weights, align } = block;
  const rows = table.map((row, index) => {
    const header = index === 0;
    return TableRow.new(id+"-row-"+index,index+1).flex().items_stretch().w_full()
      .bg(header ? colors.raised : index % 2 ? colors.surface : colors.chrome)
      .border_t(header ? 0 : 1).border_color(colors.border)
      .children(row.map((cell, col) => {
        const key = id+"-"+index+"-"+col;
        const text = richText(key+"-text","<p>"+(header?"<strong>"+cell+"</strong>":cell)+"</p>",colors.foreground);
        if (align[col] === "center") text.text_center();
        if (align[col] === "right") text.text_right();
        return (header ? TableHead : TableCell).new(key,col+1)
          .min_w(0).flex_basis(42).flex_grow(weights[col]).px(10).py(8)
          .border_l(col ? 1 : 0).border_color(colors.border).child(text);
      }));
  });
  return Table.new(id).row_count(table.length).column_count(weights.length)
    .accessibility_label("Message table").w_full().min_w(0).rounded(6).overflow_hidden()
    .border(1).border_color(colors.border)
    .child(TableHeader.new(id+"-head").child(rows[0]))
    .child(TableBody.new(id+"-body").children(rows.slice(1)));
}

export function makeProse(colors) {
  const cache=new Map();let bytes=0;
  const prose=(id,text,marks=false)=>{
    const key=id+":"+marks;let entry=cache.get(key);
    if(!entry || entry.text!==text){
      if(entry){bytes-=entry.bytes;cache.delete(key);}
      const blocks=documentBlocks(text,marks);
      const size=blocks.reduce((total, block)=>total+(block.html?.length ?? block.table.flat().reduce((sum,cell)=>sum+cell.length,0)),0);
      entry={text,blocks,bytes:2*(text.length+size)};
      while(cache.size && (cache.size>=32||bytes+entry.bytes>8*1024*1024)){const key=cache.keys().next().value;bytes-=cache.get(key).bytes;cache.delete(key);}
      if(entry.bytes<=8*1024*1024){cache.set(key,entry);bytes+=entry.bytes;}
    }
    const t = colors();
    return v_flex().w_full().min_w(0).max_w(1320).gap(12)
      .children(entry.blocks.map((block,index)=>{
        const key = id+"-"+index;
        if (block.table) return proseTable(key,block,t);
        const text = richText(key,block.html,block.quote?t.muted:t.foreground);
        return block.quote
          ? div().w_full().min_w(0).border_l(3).border_color(t.accent).bg(t.chrome).px(14).py(8).child(text)
          : text;
      }));
  };
  prose.clear=()=>{cache.clear();bytes=0;};return prose;
}
