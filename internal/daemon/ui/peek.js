// Literal passage lookup only; no inferred question or turn identity.
function compact(text){
  let value="";const starts=[],ends=[];
  for(let i=0;i<text.length;){const char=String.fromCodePoint(text.codePointAt(i)),end=i+char.length;
    if(!/\s/u.test(char))for(const part of char.toLocaleLowerCase()){value+=part;for(let n=0;n<part.length;n++){starts.push(i);ends.push(end);}}
    i=end;
  }
  return {value,starts,ends};
}
export function findPeekPassage(text,excerpt){
  if(typeof text!=="string"||typeof excerpt!=="string"||text.length>250000)return null;
  const wanted=excerpt.slice(0,32000),source=compact(text);
  const candidates=[wanted,...wanted.split("\n").filter(s=>s.trim().length>=32).slice(0,12),wanted.slice(0,220)];
  for(const [index,candidate] of candidates.entries()){const needle=compact(candidate).value;if(needle.length<(index===0?4:24))continue;const at=source.value.indexOf(needle);
    if(at!==-1)return {start:source.starts[at],end:source.ends[at+needle.length-1]};
  }
  return null;
}
export function highlightPeekPassage(root,excerpt){
  for(const mark of root.querySelectorAll("mark.nav-ask-match"))mark.replaceWith(...mark.childNodes);
  let remaining=2000000;
  // The newest literal occurrence is the useful fallback when placement is unknown.
  for(const body of [...root.querySelectorAll(".nav-feed-body")].reverse()){
    let text="";const parts=[];
    for(const line of body.querySelectorAll(".nav-md-line")){
      const walker=root.ownerDocument.createTreeWalker(line,4);let node;
      while((node=walker.nextNode())){if(node.parentElement.closest('[aria-hidden="true"]'))continue;
        parts.push({node,start:text.length,end:text.length+node.data.length});text+=node.data;
        if(text.length>250000)break;
      }
      text+="\n";if(text.length>250000)break;
    }
    remaining-=text.length;if(remaining<0)return false;
    const match=findPeekPassage(text,excerpt);if(!match)continue;
    let first=null;
    for(const part of parts){const start=Math.max(match.start,part.start)-part.start,end=Math.min(match.end,part.end)-part.start;if(end<=start)continue;
      const tail=part.node.splitText(start);tail.splitText(end-start);const mark=root.ownerDocument.createElement("mark");mark.className="nav-ask-match";tail.replaceWith(mark);mark.append(tail);first??=mark;
    }
    if(first){
      for(let parent=first.parentElement;parent&&parent!==root;parent=parent.parentElement)if(parent.tagName==="DETAILS")parent.open=true;
      first.scrollIntoView({block:"center",behavior:"instant"});return true;
    }
  }
  return false;
}
