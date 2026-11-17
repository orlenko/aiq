// Only generated formatting tags reach TextView.html. Document-supplied tags,
// URLs and image resources are text, never native HTML or network requests.
export function escapeHTML(text) {
  return String(text).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");
}
function inline(text) {
  const out=[];let i=0,plain=0;
  while (i < text.length) {
    const c=text.charCodeAt(i);if(c!==92&&c!==96&&c!==42&&c!==95){i++;continue;}
    if(i>plain)out.push(escapeHTML(text.slice(plain,i)));
    if (text[i] === "\\" && i + 1 < text.length) {
      out.push(escapeHTML(text[i + 1])); i += 2;plain=i;continue;
    }
    if (text[i] === "`") {
      let n = 1; while (text[i+n] === "`") n++;
      const end = text.indexOf("`".repeat(n), i+n);
      if (end >= 0) { out.push("<code>"+escapeHTML(text.slice(i+n,end))+"</code>"); i=end+n;plain=i;continue; }
      // Consume the whole unmatched run. Retrying its suffix one character
      // at a time makes a long literal run quadratic in the render path.
      out.push(escapeHTML(text.slice(i,i+n)));i+=n;plain=i;continue;
    }
    const mark = text.startsWith("**",i) ? "**" : text.startsWith("__",i) ? "__" : null;
    if (mark) {
      const end = text.indexOf(mark,i+2);
      if (end > i+2) { out.push("<strong>"+escapeHTML(text.slice(i+2,end))+"</strong>"); i=end+2;plain=i;continue; }
    }
    // Links remain readable, including their destination. They are deliberately
    // not resource-bearing HTML: copying retains the original Markdown.
    out.push(escapeHTML(text[i++]));plain=i;
  }
  if(i>plain)out.push(escapeHTML(text.slice(plain,i)));return out.join("");
}
const quoteLine = /^ {0,3}>[ \t]?(.*)$/;
const fenceLine = /^ {0,3}(`{3,}|~{3,})/;
const headingLine = /^ {0,3}(#{1,6})[ \t]+(\S.*)$/;
const bulletLine = /^\s*([-+*]|\d+[.)])\s+(.+)$/;
const maxTableCells = 384;

function tableCells(line) {
  if (!line?.includes("|")) return null;
  const row = line.trim();
  const cells = [];
  let start = 0;
  for (const match of row.matchAll(/\\[\\|]|\|/g)) {
    if (match[0] !== "|") continue;
    if (match.index > 0) cells.push(row.slice(start, match.index).trim());
    start = match.index + 1;
  }
  if (start === 0) return null;
  if (start < row.length) cells.push(row.slice(start).trim());
  return cells.map(cell => cell.replace(/\\([\\|])/g, (match, char) => char === "|" ? "|" : match));
}

function tableAt(lines, start, cellBudget) {
  const separator = lines[start + 1];
  if (!separator || !/^\s*[:|\-][ \t:|\-]*$/.test(separator)) return null;
  const columns = tableCells(separator);
  if (!columns?.length || !columns.every(cell => /^:?-+:?$/.test(cell))) return null;
  const header = tableCells(lines[start]);
  if (header?.length !== columns.length) return null;

  const rows = [header];
  let next = start + 2;
  for (; next < lines.length; next++) {
    const line = lines[next];
    if (quoteLine.test(line) || fenceLine.test(line) || headingLine.test(line) || bulletLine.test(line)) break;
    const cells = tableCells(line);
    if (!cells || cells.length > columns.length) break;
    rows.push(cells);
  }
  if (columns.length > 12 || rows.length * columns.length > cellBudget) {
    // Preserve every row in a readable form without creating a native view per cell.
    return { next, block: { html: lines.slice(start, next).map(line => "<p>"+inline(line)+"</p>").join("\n") } };
  }
  const weights = columns.map(() => 4);
  const table = rows.map(row => columns.map((_, col) => {
    const cell = row[col] ?? "";
    weights[col] = Math.max(weights[col], Math.min(72, cell.length));
    return inline(cell);
  }));
  const align = columns.map(cell => cell.endsWith(":") ? (cell.startsWith(":") ? "center" : "right") : "left");
  return { next, block: { table, weights, align }, cells: rows.length * columns.length };
}

function paragraphHTML(lines) {
  return "<p>"+inline(lines.join(" "))+"</p>";
}

export function documentBlocks(raw, marks = false) {
  if (marks) return [{ html: "<pre>"+escapeHTML(raw)+"</pre>" }];
  const lines = String(raw).replace(/\r\n?/g,"\n").split("\n");
  const blocks = [];
  let html = [], fence = null, code = [], paragraph = [], cellBudget = maxTableCells;
  const flush = () => {
    if (paragraph.length) {
      html.push(paragraphHTML(paragraph));
      paragraph = [];
    }
  };
  const flushBlock = () => {
    flush();
    if (html.length) {
      blocks.push({ html: html.join("\n") });
      html = [];
    }
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const f = fenceLine.exec(line);
    if (f && (!fence || f[1][0] === fence[0] && f[1].length >= fence.length)) {
      flush();
      if (fence) { html.push("<pre><code>"+escapeHTML(code.join("\n"))+"</code></pre>"); fence=null; code=[]; }
      else fence=f[1];
      continue;
    }
    if (fence) { code.push(line); continue; }
    if (!line.trim()) { flush(); continue; }
    const heading = headingLine.exec(line);
    const bullet = bulletLine.exec(line);
    const quote = quoteLine.exec(line);
    if (quote) {
      flushBlock();
      const paragraphs = [];
      let quoted = [], match = quote;
      while (match) {
        if (match[1].trim()) quoted.push(match[1]);
        else if (quoted.length) {
          paragraphs.push(paragraphHTML(quoted));
          quoted = [];
        }
        match = quoteLine.exec(lines[++i] ?? "");
      }
      if (quoted.length) paragraphs.push(paragraphHTML(quoted));
      if (paragraphs.length) blocks.push({ html: paragraphs.join("\n"), quote: true });
      i--;
    } else if (heading || bullet) {
      flush();
      if (heading) {
        const tag = "h"+heading[1].length;
        const title = heading[2].trimEnd().replace(/(?:^|[ \t])#+$/, "").trimEnd();
        html.push("<"+tag+">"+inline(title)+"</"+tag+">");
      } else {
        const marker = bullet[1].length > 1 ? bullet[1] : "•";
        html.push("<p>"+marker+" "+inline(bullet[2])+"</p>");
      }
    } else {
      const table = tableAt(lines, i, cellBudget);
      if (table) {
        flushBlock();
        blocks.push(table.block);
        cellBudget -= table.cells ?? 0;
        i = table.next - 1;
      } else paragraph.push(line);
    }
  }
  if (fence) html.push("<pre><code>"+escapeHTML(code.join("\n"))+"</code></pre>");
  flushBlock();
  return blocks;
}
