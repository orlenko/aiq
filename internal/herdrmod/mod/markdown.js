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
export function documentHTML(raw, marks = false) {
  if (marks) return "<pre>"+escapeHTML(raw)+"</pre>";
  const lines = String(raw).replace(/\r\n?/g,"\n").split("\n");
  const out = []; let fence = null, code = [], paragraph = [];
  const flush = () => { if (paragraph.length) { out.push("<p>"+paragraph.map(inline).join("<br>")+"</p>"); paragraph=[]; } };
  for (const line of lines) {
    const f = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (f && (!fence || f[1][0] === fence[0] && f[1].length >= fence.length)) {
      flush();
      if (fence) { out.push("<pre><code>"+escapeHTML(code.join("\n"))+"</code></pre>"); fence=null; code=[]; }
      else fence=f[1];
      continue;
    }
    if (fence) { code.push(line); continue; }
    if (!line.trim()) { flush(); continue; }
    const heading = /^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$/.exec(line);
    const bullet = /^\s*(?:[-+*]|\d+[.)])\s+(.+)$/.exec(line);
    const quote = /^\s*>\s?(.*)$/.exec(line);
    if (heading || bullet || quote) {
      flush();
      out.push(heading ? "<p><strong>"+inline(heading[1])+"</strong></p>" : bullet ? "<p>• "+inline(bullet[1])+"</p>" : "<blockquote>"+inline(quote[1])+"</blockquote>");
    } else paragraph.push(line);
  }
  if (fence) out.push("<pre><code>"+escapeHTML(code.join("\n"))+"</code></pre>");
  flush(); return out.join("\n");
}
