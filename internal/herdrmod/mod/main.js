import { View, div } from "gpui-kit";
import { v_flex, h_flex, InputState, Input, ScrollHandle, Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from "gpui-base";
import { exec, hc, machines, panes, focused, run, theme, checking, nextCommand } from "herdr";
import { Bridge, matchesTarget } from "./protocol.js";
import { Selection, machineKey, sessionKey, rowKey, groups, localTime, errorText } from "./state.js";
import { button, label, panel, makeProse } from "./ui.js";

export default class AIQ extends View {
  init(_props,cx) {
    this.bridge=new Bridge(exec);this.sources=new Map();this.selection=new Selection();this.prose=makeProse();
    this.tab="asks";this.machine="all";this.section="pending";this.query="";this.marks=false;this.hasAsks=false;this.liveOnly=false;this.workers=false;
    this.detail=null;this.context=null;this.conversation=null;this.busy=false;this.notice="";this.undo=[];this.sort="updated_ms";this.desc=true;this.size={width:1200,height:800};
    this.scroll=ScrollHandle.new();this.contextScroll=ScrollHandle.new();this.handle=cx.focus_handle();
    this.filter=InputState.new({placeholder:"Filter sessions, projects or asks"});
    this.filter.on("change",(_e,cx)=>{this.query=this.filter.value();this.reconcile(cx);cx.notify();});
    this.search=InputState.new({placeholder:"Find in loaded conversation"});
    this.search.on("submit",(_e,cx)=>this.findContext(cx));
    if(!checking()) {
      this.refresh(cx);
      cx.timer.every(30000,cx=>{if(this.visible() && !this.busy && this.tab==="asks")this.refresh(cx);});
      cx.spawn(async cx=>{for(;;){const command=await nextCommand();if(command==="activate"){
        const pane=panes().find(p=>p.mod==="aiq");if(pane)run("pane.focus",{pane:pane.pane});this.handle.focus();cx.notify();
      }else if(command==="focus"){this.handle.focus();}}});
    }
  }
  visible() { const copies=panes().filter(p=>p.mod==="aiq");return copies.length===1 && copies[0].tab===focused()?.tab; }
  sourceList() { return [...this.sources.values()].filter(s=>this.machine==="all" || machineKey(s.machine)===this.machine); }
  cache(s,kind) { return s[kind]??{loading:false,error:null,value:null}; }
  askRows() {
    const seen=new Set(),rows=[];
    for(const s of this.sourceList()) {
      const e=s.asks?.value;if(!e || seen.has(e.host.id))continue;seen.add(e.host.id);
      for(const r of e.data.rows)rows.push({...r,machine:s.machine,host:e.host.id});
    }return rows;
  }
  groupList(){return groups(this.askRows(),this.query,this.section);}
  current(){return this.askRows().find(r=>rowKey(r)===this.selection.ask)??null;}
  reconcile(cx) {
    const before=this.selection.ask,row=this.selection.reconcile(this.groupList());
    const changed=!!row && this.detail?.revision && this.detail.revision!==row.revision;
    if(changed){this.selection.generation++;this.selection.peek=null;}
    if(before!==this.selection.ask || changed){this.detail=null;this.context=null;if(row)this.loadDetail(row,cx);}
  }
  setTab(tab,cx){this.tab=tab;this.conversation=null;this.notice="";this.prose.clear();this.refresh(cx);cx.notify();}
  refresh(cx) {
    if(checking())return;
    const current=machines();
    if(!current.some(m=>m.name===null))current.unshift({name:null,layout:"ready"});
    for(const m of current) {
      const key=machineKey(m.name);
      if(!this.sources.has(key))this.sources.set(key,{machine:m.name,layout:m.layout});
      this.sources.get(key).layout=m.layout;
    }
    for(const s of this.sourceList())this.load(s,this.tab,cx);
  }
  load(s,kind,cx,options={},append=false) {
    const old=this.cache(s,kind);if(old.loading || checking())return;
    const token={};s[kind]={...old,loading:true,error:null,token};cx.notify();
    cx.spawn(async cx=>{
      try {
        const e=await this.bridge.read(s.machine,kind,options,()=>s[kind]?.token===token);
        if(s[kind]?.token!==token)return;
        if(append && old.value) e.data.rows=[...old.value.data.rows,...e.data.rows];
        s[kind]={value:e,loading:false,error:null};
      }catch(e){
        if(s[kind]?.token!==token)return;
        // Source identity changed: old rows cannot authorize writes on a new DB.
        if(e.code==="source_changed") {
          for(const key of ["asks","sessions","quotas"])s[key]={value:null,loading:false,error:"source_changed",token:null};
          this.notice=errorText("source_changed");
        } else s[kind]={value:old.value,loading:false,error:e.code??"unavailable"};
      }
      if(kind==="asks")this.reconcile(cx);cx.notify();
    });
  }
  choose(row,cx) {
    this.selection.choose(row);this.detail=null;this.context=null;this.notice="";this.search.set_value("");
    this.prose.clear();
    this.loadDetail(row,cx);cx.notify();
  }
  loadDetail(row,cx) {
    if(!row || checking())return;
    const generation=this.selection.generation;this.detail={loading:true,revision:row.revision};
    cx.spawn(async cx=>{try {
      const e=await this.bridge.read(row.machine,"ask-detail",row,()=>generation===this.selection.generation);
      if(generation!==this.selection.generation)return;
      if(e.data.id!==row.id || e.data.revision!==row.revision)throw {code:"conflict"};
      this.detail={value:e.data,loading:false,revision:row.revision};
    }catch(e){if(generation!==this.selection.generation)return;this.detail={loading:false,revision:row.revision,error:e.code??"unavailable"};}cx.notify();});
  }
  peek(row,cx) {
    if(checking())return;
    if(this.selection.peek===rowKey(row)){this.selection.peek=null;this.context=null;cx.notify();return;}
    const token={};this.selection.peek=rowKey(row);this.context={loading:true,token};
    const generation=this.selection.generation;
    cx.spawn(async cx=>{try{
      const e=await this.bridge.read(row.machine,"ask-context",row,()=>generation===this.selection.generation&&this.context?.token===token);
      if(generation!==this.selection.generation || this.selection.peek!==rowKey(row) || this.context?.token!==token)return;
      if(e.data.id!==row.id || e.data.revision!==row.revision)throw {code:"conflict"};
      this.context={loading:false,value:e.data};
      const index=e.data.messages.findIndex(m=>m.anchor);
      if(index>=0)this.contextScroll.scroll_to_item(index);
    }catch(e){if(generation!==this.selection.generation || this.selection.peek!==rowKey(row) || this.context?.token!==token)return;this.context={loading:false,error:e.code??"unavailable"};}cx.notify();});
  }
  findContext(cx) {
    const data=this.conversation?.value??this.context?.value,q=this.search.value().toLowerCase();
    if(!data || !q)return;
    const index=data.messages.findIndex(m=>m.text.toLowerCase().includes(q));
    if(index>=0){this.match=data.messages[index].key;(this.conversation?this.scroll:this.contextScroll).scroll_to_item(index);this.notice="Match in loaded messages.";}
    else this.notice="No match in this loaded window. Load earlier or later messages to search more.";
    cx.notify();
  }
  async reloadAsks(machine) {
    const s=this.sources.get(machineKey(machine)),e=await this.bridge.read(machine,"asks");
    s.asks={value:e,loading:false,error:null};return e;
  }
  dismiss(rows,cx,action="dismiss") {
    if(this.busy || checking() || !rows.length)return;
    this.busy=true;this.notice="";const snapshot=rows.map(r=>({...r}));
    for(const machine of new Set(snapshot.map(r=>r.machine))){const s=this.sources.get(machineKey(machine));if(s?.asks)s.asks={...s.asks,token:null,loading:false};}
    cx.spawn(async cx=>{
      const undo=[];let stopped=false;
      for(const row of snapshot) {
        try {
          const e=await this.bridge.action(row.machine,row,action);
          if(e.data.id!==row.id)throw {code:"unknown_outcome"};
          undo.push({...row,revision:e.data.revision,state:e.data.state});
        }catch(e){
          stopped=true;
          if(e.code==="conflict" || e.code==="source_changed")this.notice=errorText(e.code);
          else {
            // Resolve once against the same source, without repeating the write.
            try {
              const fresh=await this.reloadAsks(row.machine);
              if(fresh.host.id!==row.host)throw {code:"source_changed"};
              const now=fresh.data.rows.find(r=>r.id===row.id);
              if(now?.state===(action==="dismiss"?"dismissed":"open")) {undo.push({...row,revision:now.revision,state:now.state});this.notice="Action confirmed by refresh. The remaining batch was stopped.";}
              else this.notice=now?.revision===row.revision?"The action was not applied. The remaining batch was stopped.":errorText("unknown_outcome");
            }catch{this.notice=errorText("unknown_outcome");}
          }
          break;
        }
      }
      if(action==="dismiss")this.undo=undo;
      else this.undo=[];
      for(const machine of new Set(snapshot.map(r=>r.machine))){try{await this.reloadAsks(machine);}catch(e){const s=this.sources.get(machineKey(machine));s.asks={...s.asks,error:e.code??"unavailable",loading:false};}}
      if(!stopped)this.notice=action==="dismiss"?`${undo.length} ask${undo.length===1?"":"s"} dismissed.`:"Dismissal restored.";
      this.busy=false;this.reconcile(cx);cx.notify();
    });
  }
  openConversation(row,cx,options={}) {
    if(checking())return;
    const token={};this.conversation={loading:true,token,row};this.match=null;cx.notify();
    cx.spawn(async cx=>{try{
      const e=await this.bridge.read(row.machine,"conversation",{provider:row.provider,id:row.id,host:row.host,...options},()=>this.conversation?.token===token);
      if(this.conversation?.token!==token)return;
      this.conversation={loading:false,value:e.data,row};
      const index=e.data.messages.findIndex(m=>m.anchor);this.scroll.scroll_to_item(index>=0?index:Math.max(0,e.data.messages.length-1));
    }catch(e){if(this.conversation?.token!==token)return;this.conversation={loading:false,error:e.code??"unavailable",row};}cx.notify();});
  }
  focusTerminal(row,cx) {
    if(checking() || this.busy)return;
    this.busy=true;cx.spawn(async cx=>{try{
      // Ask actions use an exact row revision. Sessions resolve by provider/id.
      const e=await this.bridge.read(row.machine,"target",row);
      const target=e.data.terminal;if(!target)throw {code:"not_found"};
      const address=(row.machine?row.machine+"/":"")+target.address;
      const r=await hc(["get",address,"--json"]);
      if(r.code!==0)throw {code:"unavailable"};
      let live;try{live=JSON.parse(r.stdout);}catch{throw {code:"internal"};}
      if(!matchesTarget(target,live))throw {code:"not_found"};
      const visible=panes().find(p=>p.terminal===address);
      run(visible?"pane.focus":"terminal.peek",{terminal:address});
      this.notice=target.relation==="workspace_current"?"Opened the workspace’s current terminal; this ask is from an earlier conversation.":"Opened the session terminal.";
    }catch(e){this.notice=errorText(e.code??"unavailable");}this.busy=false;cx.notify();});
  }
  status(s,kind) {
    const c=this.cache(s,kind),name=s.machine??"Here";
    const status=c.error?errorText(c.error):c.loading?"Refreshing…":kind==="asks"&&c.value?.data.setup==="not_set_up"?"Asks not enabled":c.value?.partial?"Partial data · updated "+localTime(c.value.generated_at_ms):c.value?"Updated "+localTime(c.value.generated_at_ms):s.layout==="unavailable"?"Herdr connection unavailable":"Not loaded";
    return label(name+" · "+status+(c.error&&c.value?" · showing last known data":""),true);
  }
  toolbar(cx) {
    return h_flex().gap(12).items_center().px(24).py(14).border_b(1).border_color(theme().border).children([
      div().font_semibold().text_size(20).child("aiq"),
      ...["asks","sessions","quotas"].map(tab=>button("tab-"+tab,tab[0].toUpperCase()+tab.slice(1),(_e,cx)=>this.setTab(tab,cx),this.tab===tab)),
      div().flex_1(),
      button("marks",this.marks?"Markdown symbols ✓":"Markdown symbols",(_e,cx)=>{this.marks=!this.marks;cx.notify();},this.marks),
      button("refresh","Refresh",(_e,cx)=>this.refresh(cx))
    ]);
  }
  render(cx) {
    const t=theme();
    return v_flex().id("root").size_full().min_h(0).bg(t.surface).text_color(t.foreground).text_size(14).track_focus(this.handle)
      .on_size_change((size,cx)=>{if(size.width!==this.size.width||size.height!==this.size.height){this.size=size;cx.notify();}})
      .on_key_down((e,cx)=>{
        if(e.keystroke==="cmd-shift-m"){this.marks=!this.marks;cx.stop_propagation();cx.notify();}
        if(e.key==="escape" && this.selection.peek){this.selection.peek=null;this.context=null;cx.stop_propagation();cx.notify();}
      }).children([
        this.toolbar(cx),
        h_flex().px(24).py(8).gap(8).items_center().children([
          label("Machines",true),...["all",...this.sources.keys()].map(key=>button("machine-"+key,key==="all"?"All":key||"Here",(_e,cx)=>{this.machine=key;this.reconcile(cx);this.refresh(cx);cx.notify();},this.machine===key))
        ]),
        h_flex().px(24).py(4).gap(18).flex_wrap().children(this.sourceList().map(s=>this.status(s,this.tab))),
        this.notice||this.undo.length?h_flex().px(24).py(8).gap(12).items_center().children([label(this.notice||`${this.undo.length} dismissal${this.undo.length===1?"":"s"} can be undone.`),...(this.undo.length?[button("undo","Undo",(_e,cx)=>this.dismiss(this.undo,cx,"restore"),false,this.busy)]:[])]):null,
        this.conversation?this.conversationView(cx):this.tab==="asks"?this.asksView(cx):this.tab==="sessions"?this.sessionsView(cx):this.quotasView(cx)
      ].filter(Boolean));
  }
  asksView(cx) {
    const list=this.groupList(),row=this.current(),group=list.find(g=>g.key===this.selection.group),t=theme();
    return h_flex().flex_1().h(Math.max(300,this.size.height-150)).min_h(0).items_stretch().children([
      v_flex().h_full().w(320).min_w(260).border_r(1).border_color(t.border).min_h(0).children([
        v_flex().p(16).gap(10).children([
          Input.new(this.filter).border(1).border_color(t.border).p(8).rounded(5),
          h_flex().gap(8).children([button("pending","Pending",(_e,cx)=>{this.section="pending";this.reconcile(cx);cx.notify();},this.section==="pending"),button("done","Done / dismissed",(_e,cx)=>{this.section="done";this.reconcile(cx);cx.notify();},this.section==="done")]),
          label(`${list.length} sessions · ${list.reduce((n,g)=>n+g.rows.length,0)} loaded asks`,true)
        ]),
        v_flex().id("session-groups").flex_1().min_h(0).overflow_y_scrollbar().px(8).gap(5).children(list.map(g=>
          v_flex().id("group-"+g.key).p(12).gap(5).rounded(6).border(1).border_color(g.key===this.selection.group?t.accent:t.border).bg(g.key===this.selection.group?t.raised:t.surface).on_click((_e,cx)=>this.choose(g.rows[0],cx)).children([
            h_flex().justify_between().children([div().font_semibold().child(g.workspace.split("/").filter(Boolean).pop()||"Unknown workspace"),label(g.rows.length)]),
            label(g.name||g.provider+" · "+g.rows[0].conversation_id.slice(0,8),true),label((g.machine??"Here")+" · "+localTime(g.updated),true),label(g.workspace,true)
          ]))),
        !list.length?div().p(16).child("No matching asks in the loaded data."):null
      ].filter(Boolean)),
      row&&group?this.askView(row,group,cx):panel("ask-empty").child(label("Choose a session to review its asks.",true))
    ]);
  }
  askView(row,group,cx) {
    const index=group.rows.findIndex(r=>rowKey(r)===rowKey(row)),peeking=this.selection.peek===rowKey(row),data=this.detail?.value;
    const dismissable=group.rows.filter(r=>r.state==="open"||r.state==="probably-done");
    const restore=row.state==="dismissed"||row.state==="closed";
    const question=v_flex().h_full().flex_1().min_w(0).min_h(0).children([
      v_flex().px(24).py(16).gap(10).border_b(1).border_color(theme().border).children([
        h_flex().gap(8).items_center().children([
          div().flex_1().font_semibold().child(group.workspace),
          button("prev","←",(_e,cx)=>this.choose(group.rows[index-1],cx),false,index<=0),label(`${index+1} / ${group.rows.length}`),button("next","→",(_e,cx)=>this.choose(group.rows[index+1],cx),false,index>=group.rows.length-1)
        ]),
        label(`${row.provider} · ${row.conversation_id.slice(0,8)} · ${row.machine??"Here"}`,true),
        ...(dismissable.length?[h_flex().gap(8).children([button("batch",`Dismiss ${dismissable.length} session asks`,(_e,cx)=>this.dismiss(dismissable,cx),false,this.busy)])]:[])
      ]),
      panel("question").child(v_flex().gap(16).children([
        label(`Asked ${localTime(row.raised_at_ms)} · Ask ${row.id}`,true),
        this.detail?.loading?label("Loading full ask…",true):null,
        this.detail?.error?label(errorText(this.detail.error),true):null,
        this.prose("ask-text-"+row.id,data?.latest.text??row.latest.preview,this.marks),
        (data?.latest.truncated || !data&&row.latest.truncated)?label("This excerpt is truncated.",true):null,
        h_flex().gap(8).flex_wrap().children([
          button("dismiss",restore?"Restore":"Dismiss",(_e,cx)=>this.dismiss([row],cx,restore?"restore":"dismiss"),true,this.busy),
          ...(row.state==="probably-done"?[button("reopen","Reopen ask",(_e,cx)=>this.dismiss([row],cx,"restore"),false,this.busy)]:[]),
          button("peek",peeking?"Close context":"Peek at original context",(_e,cx)=>this.peek(row,cx),peeking),
          button("copy","Copy ask",(_e,cx)=>cx.write_to_clipboard(data?.latest.text??row.latest.preview),false,!data),
          button("terminal",row.terminal?.relation==="workspace_current"?"Current workspace terminal":"Go to terminal",(_e,cx)=>this.focusTerminal(row,cx),false,this.busy||!row.terminal)
        ]),
        !row.terminal?label("Terminal unavailable · "+(row.terminal_reason??"unknown"),true):null,
        label("Reply in the session’s terminal.",true)
      ].filter(Boolean)))
    ]);
    return h_flex().h_full().items_stretch().flex_1().min_w(0).min_h(0).children([question,...(peeking?[this.contextView(row,cx)]:[])]);
  }
  contextView(row,cx) {
    const c=this.context,t=theme();
    return v_flex().h_full().w(Math.max(360,this.size.width*0.43)).min_h(0).border_l(1).border_color(t.border).children([
      v_flex().px(24).py(16).gap(10).border_b(1).border_color(t.border).children([
        h_flex().gap(8).items_center().children([div().font_semibold().flex_1().child("Original context"),button("close-context","Close",(_e,cx)=>{this.selection.peek=null;this.context=null;cx.notify();})]),
        Input.new(this.search).border(1).border_color(t.border).p(8).rounded(5),
        label(c?.value?.placement?.confidence==="time+text"?"Matched ask highlighted below":c?.value?.placement?.confidence==="time"?"Located by timestamp; check the highlighted message":"Original turn not located",true)
      ]),
      c?.loading?panel("context-loading").child("Loading conversation…"):c?.error?panel("context-error").child(errorText(c.error)):this.messageList(c?.value,this.contextScroll,"context-messages"),
      c?.value?this.pageControls(c.value,{machine:row.machine,host:row.host,provider:row.provider,id:row.conversation_id},cx,true):null
    ].filter(Boolean));
  }
  messageList(data,scroll,id) {
    if(!data?.messages.length)return panel(id).child(label("No messages in this window.",true));
    const t=theme();
    return v_flex().id(id).flex_1().min_h(0).overflow_y_scrollbar().track_scroll(scroll).p(24).gap(16).children(data.messages.map(m=>
      v_flex().id(id+"-"+m.key).p(16).gap(10).border(1).border_color(m.anchor||this.match===m.key?t.accent:t.border).rounded(6).bg(m.anchor||this.match===m.key?t.raised:t.surface).children([
        label(`${m.role} · ${localTime(m.at_ms)}${m.anchor?" · matching ask":""}`,true),this.prose(id+"-text-"+m.key,m.text,this.marks),...(m.truncated?[label("Message truncated at its display limit.",true)]:[])
      ])));
  }
  pageControls(data,row,cx,context=false) {
    return h_flex().px(24).py(12).gap(8).children([
      button("earlier","Earlier messages",(_e,cx)=>this.openConversation(row,cx,{before:data.cursor.before}),false,!data.cursor?.before),
      button("later","Later messages",(_e,cx)=>this.openConversation(row,cx,{after:data.cursor.after}),false,!data.cursor?.after),
      ...(context?[button("full","Full conversation",(_e,cx)=>this.openConversation(row,cx,{at:data.placement?.anchor_ms}))]:[])
    ]);
  }
  conversationView(cx) {
    const c=this.conversation;
    return v_flex().h(Math.max(300,this.size.height-150)).flex_1().min_h(0).children([
      h_flex().px(24).py(14).gap(12).items_center().children([
        button("back","← Back",(_e,cx)=>{this.conversation=null;cx.notify();}),label(c.row.name||c.row.cwd||c.row.id),div().flex_1(),Input.new(this.search).w(300).border(1).border_color(theme().border).p(8)
      ]),
      c.loading?panel("conversation-loading").child("Loading conversation…"):c.error?panel("conversation-error").child(errorText(c.error)):this.messageList(c.value,this.scroll,"conversation"),
      c.value?this.pageControls(c.value,c.row,cx):null
    ].filter(Boolean));
  }
  sessionsView(cx) {
    let rows=[];const seen=new Set(),q=this.query.toLowerCase();
    for(const s of this.sourceList()){const e=s.sessions?.value;if(!e||seen.has(e.host.id))continue;seen.add(e.host.id);rows.push(...e.data.rows.map(r=>({...r,machine:s.machine,host:e.host.id})));}
    rows=rows.filter(r=>[r.name,r.cwd,r.provider,r.machine??"Here",r.live].join(" ").toLowerCase().includes(q));
    rows=rows.filter(r=>(this.workers||!r.worker)&&(!this.hasAsks||r.pending_asks_known&&r.pending_asks>0)&&(!this.liveOnly||r.live==="live"));
    const direction=this.desc?-1:1,key=this.sort;rows.sort((a,b)=>direction*(typeof a[key]==="number"?a[key]-b[key]:String(a[key]??"").localeCompare(String(b[key]??""))));
    const cols=[["name","Session"],["cwd","Workspace"],["machine","Machine"],["provider","Provider"],["pending_asks","Asks"],["updated_ms","Last activity"],["live","Status"]];
    return v_flex().h(Math.max(300,this.size.height-150)).flex_1().min_h(0).px(24).py(16).gap(14).children([
      h_flex().gap(8).items_center().flex_wrap().children([
        Input.new(this.filter).w(320).border(1).border_color(theme().border).p(8),
        ...[["hasAsks","Has asks"],["liveOnly","Live only"],["workers","Include workers"]].map(([key,text])=>button("filter-"+key,text,(_e,cx)=>{this[key]=!this[key];cx.notify();},this[key])),
        label(`${rows.length} loaded rows`,true),div().flex_1(),
        ...this.sourceList().filter(s=>s.sessions?.value?.data.window.cursor).map(s=>button("older-"+machineKey(s.machine),"Older · "+(s.machine??"Here"),(_e,cx)=>this.load(s,"sessions",cx,{before:s.sessions.value.data.window.cursor},true),false,s.sessions.loading))
      ]),
      div().id("sessions-table-scroll").min_h(0).flex_1().overflow_y_scrollbar().child(
        Table.new("sessions-grid").w_full().accessibility_label("Sessions across machines").row_count(rows.length+1).column_count(cols.length).children([
          TableHeader.new("headers").child(TableRow.new("header-row",1).children(cols.map(([key,title],i)=>TableHead.new("head-"+key,i+1).p(10).child(button("sort-"+key,title+(this.sort===key?(this.desc?" ↓":" ↑"):""),(_e,cx)=>{this.desc=this.sort===key?!this.desc:true;this.sort=key;cx.notify();}))))),
          TableBody.new("session-body").children(rows.map((r,i)=>TableRow.new("session-"+r.host+"-"+r.provider+"-"+(r.id||r.cwd),i+2).children(cols.map(([key],j)=>{
            let value=key==="name"?(r.name||r.id.slice(0,8)||"Workspace history"):key==="machine"?(r.machine??"Here"):key==="pending_asks"?(r.pending_asks_known?String(r.pending_asks):"Unknown"):key==="updated_ms"?localTime(r.updated_ms):r[key];
            return TableCell.new("cell-"+i+"-"+j,j+1).p(10).border_b(1).border_color(theme().border).child(key==="name"?button("open-session-"+i,String(value),(_e,cx)=>this.openConversation(r,cx),false,!r.id):label(value));
          }))))
        ]))
    ]);
  }
  quotasView(cx) {
    const t=theme();
    return v_flex().id("quotas-scroll").h(Math.max(300,this.size.height-150)).flex_1().min_h(0).overflow_y_scrollbar().px(24).py(16).gap(24).children(this.sourceList().map(s=>{
      const e=s.quotas?.value;
      return v_flex().gap(12).children([
        div().font_semibold().text_size(18).child(s.machine??"Here"),
        ...(e?[h_flex().gap(16).items_start().children(["agy","claude","codex"].map(provider=>v_flex().flex_1().min_w(0).gap(10).children([
          div().text_color(t.muted).font_semibold().child(provider.toUpperCase()),
          ...e.data.accounts.filter(a=>a.provider===provider).map(a=>v_flex().p(16).gap(10).rounded(6).border(1).border_color(t.border).children([
            h_flex().justify_between().children([div().font_semibold().child(a.id.startsWith(provider+"/")?a.id:provider+"/"+a.id),label(a.rank&&a.score!==null?`#${a.rank} · ${Number(a.score).toFixed(1)}/h`:"Unranked",true)]),
            label([a.plan,a.label||a.identity].filter(Boolean).join(" · "),true),
            ...a.windows.map(w=>v_flex().gap(5).children([
              h_flex().justify_between().children([label(w.label),label(w.used_pct===null||w.used_pct<0?"Unknown":w.used_pct.toFixed(0)+"%")]),
              div().h(6).rounded(3).bg(t.raised).child(div().h(6).rounded(3).w(Math.max(0,Math.min(100,w.used_pct))+"%").bg(w.used_pct>=95?t.ansi1:w.used_pct>=80?t.ansi3:t.ansi2)),
              label("Resets "+localTime(w.resets_at_ms),true)
            ])),
            a.reset_credits?label(a.reset_credits+" reset credits",true):null,
            !a.eligible?label(a.ineligible||a.reason||"Currently ineligible",true):null,
            ...a.leases.map(l=>label(`${l.mode} · ${l.workspace||l.cwd}`,true)),
            label("Read "+localTime(a.observed_at_ms),true),
            a.poll_error?label("Latest quota read failed.",true):null
          ].filter(Boolean)))
        ])))]:[label("No quota snapshot available.",true)])
      ]);
    }));
  }
}
