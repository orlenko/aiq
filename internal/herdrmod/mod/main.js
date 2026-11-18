import { View, div } from "gpui-kit";
import { v_flex, h_flex, h_resizable, resizable_panel, InputState, Popover, ScrollHandle, VirtualListScrollHandle, v_virtual_list, Scrollbar, Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from "gpui-base";
import { exec, hc, machines, panes, focused, run, checking, nextCommand } from "herdr";
import { Bridge, matchesTarget } from "./protocol.js";
import { Selection, machineKey, sessionKey, rowKey, groups, localTime, errorText, mergeSessionRows, terminalText, omissionNotice, quotaResets, quotaResetGrant, quotaRenewals } from "./state.js";
import { appearances, createUI, loadAppearance, saveAppearance, makeProse } from "./ui.js";

// Each card has four 20px lines, three 4px gaps, 24px of padding and a border.
const askCardHeight = 120;
const askCardMargin = 3;

export default class AIQ extends View {
  init(_props,cx) {
    this.appearance = loadAppearance();
    this.appearanceOpen = false;
    this.ui = createUI(() => this.appearance);
    this.prose = makeProse(this.ui.colors);
    this.bridge=new Bridge(exec);this.sources=new Map();this.selection=new Selection();
    this.tab="asks";this.machine="all";this.section="pending";this.query="";this.marks=false;this.hasAsks=false;this.liveOnly=false;this.workers=false;this.tablePage=0;
    this.detail=null;this.context=null;this.conversation=null;this.busy=false;this.notice="";this.undo=[];this.sort="updated_ms";this.desc=true;this.size={width:1200,height:800};
    this.scroll=ScrollHandle.new();this.contextScroll=ScrollHandle.new();this.handle=cx.focus_handle();
    this.gridScroll=VirtualListScrollHandle.new();this.groupsScroll=VirtualListScrollHandle.new();this.gridFocus=cx.focus_handle();this.gridHeight=400;this.gridWidth=800;this.groupsHeight=400;this.groupsWidth=320;this.tableSelected=null;
    this.filter=InputState.new({placeholder:"Filter sessions, projects or asks"});
    this.filter.on("change",(_e,cx)=>{this.query=this.filter.value();this.tablePage=0;this.reconcile(cx);cx.notify();});
    this.search=InputState.new({placeholder:"Find in loaded conversation"});
    this.search.on("submit",(_e,cx)=>this.findContext(cx));
    if(!checking()) {
      this.refresh(cx);
      cx.timer.every(30000,cx=>{if(this.visible() && !this.busy && this.tab==="asks")this.refresh(cx);});
      cx.timer.every(60000,cx=>{if(this.visible() && !this.busy && this.tab==="quotas")this.refresh(cx);});
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
    const before=this.selection.ask,beforeGroup=this.selection.group,list=this.groupList(),row=this.selection.reconcile(list);
    if(beforeGroup!==this.selection.group){const index=list.findIndex(g=>g.key===this.selection.group);if(index>=0)this.groupsScroll.scroll_to_item(index,"center");}
    const changed=!!row && this.detail?.revision && this.detail.revision!==row.revision;
    if(changed){this.selection.generation++;this.selection.peek=null;}
    if(before!==this.selection.ask || changed){this.detail=null;this.context=null;this.match=null;this.search.set_value("");this.prose.clear();if(row)this.loadDetail(row,cx);}
  }
  setTab(tab,cx){this.tab=tab;this.conversation=null;this.selection.peek=null;this.context=null;this.notice="";this.prose.clear();this.refresh(cx);cx.notify();}
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
        const e=await this.bridge.read(s.machine,kind,options,()=>s[kind]?.token===token&&this.tab===kind&&this.sourceList().includes(s));
        if(s[kind]?.token!==token)return;
        if(append && old.value) e.data.rows=mergeSessionRows(old.value.data.rows,e.data.rows);
        s[kind]={value:e,loading:false,error:null};
      }catch(e){
        if(s[kind]?.token!==token)return;
        // Source identity changed: old rows cannot authorize writes on a new DB.
        if(e.code==="superseded")s[kind]={value:old.value,loading:false,error:old.error??null};
        else if(e.code==="source_changed") {
          for(const key of ["asks","sessions","quotas"])s[key]={value:null,loading:false,error:"source_changed",token:null};
          this.notice=errorText("source_changed");
        } else s[kind]={value:old.value,loading:false,error:e.code??"unavailable"};
      }
      if(kind==="asks")this.reconcile(cx);cx.notify();
    });
  }
  choose(row,cx) {
    this.selection.choose(row);this.detail=null;this.context=null;this.match=null;this.notice="";this.search.set_value("");
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
    const token={};this.selection.peek=rowKey(row);this.context={loading:true,token};this.match=null;
    const generation=this.selection.generation;
    cx.spawn(async cx=>{try{
      const e=await this.bridge.read(row.machine,"ask-context",row,()=>generation===this.selection.generation&&this.context?.token===token);
      if(generation!==this.selection.generation || this.selection.peek!==rowKey(row) || this.context?.token!==token)return;
      if(e.data.id!==row.id || e.data.revision!==row.revision)throw {code:"conflict"};
      this.context={loading:false,value:e.data,omissions:omissionNotice(e)};
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
      this.conversation={loading:false,value:e.data,row,omissions:omissionNotice(e)};
      const index=e.data.messages.findIndex(m=>m.anchor);this.scroll.scroll_to_item(index>=0?index:Math.max(0,e.data.messages.length-1));
    }catch(e){if(this.conversation?.token!==token)return;this.conversation={loading:false,error:e.code??"unavailable",row};}cx.notify();});
  }
  focusTerminal(row,cx) {
    if(checking() || this.busy)return;
    this.busy=true;cx.spawn(async cx=>{try{
      // Ask actions use an exact row revision. Sessions resolve by provider/id.
      const e=await this.bridge.read(row.machine,"target",row);
      const target=e.data.terminal;if(!target)throw {terminalReason:e.data.terminal_reason||"unknown"};
      const address=(row.machine?row.machine+"/":"")+target.address;
      const r=await hc(["get",address,"--json"]);
      if(r.code!==0)throw {code:"unavailable"};
      let live;try{live=JSON.parse(r.stdout);}catch{throw {code:"internal"};}
      if(!matchesTarget(target,live,row.machine))throw {code:"not_found"};
      const visible=panes().find(p=>p.terminal===address);
      run(visible?"pane.focus":"terminal.peek",{terminal:address});
      this.notice=target.relation==="workspace_current"?"Opened the workspace’s current terminal.":"Opened the session terminal.";
    }catch(e){this.notice=e.terminalReason?terminalText(e.terminalReason):errorText(e.code??"unavailable");}this.busy=false;cx.notify();});
  }
  showSessionAsks(row,cx){
    this.tab="asks";this.conversation=null;this.section="pending";this.machine=machineKey(row.machine);this.query=row.id?"":row.workspace||row.cwd||"";this.filter.set_value(this.query);
    const match=this.askRows().find(a=>a.host===row.host&&a.state==="open"&&a.provider===row.provider&&(row.id?a.conversation_id===row.id:a.workspace===(row.workspace||row.cwd)));
    if(match)this.choose(match,cx);else this.reconcile(cx);this.refresh(cx);cx.notify();
  }
  status(s,kind) {
    const c=this.cache(s,kind),name=s.machine??"Here";
    const omissions=omissionNotice(c.value),older=c.value?.omitted?.older;
    const paging=Number.isSafeInteger(older)&&older>0&&c.value?.data.window?.cursor?`${older} more transcripts available`:"";
    let ready = c.value ? "Updated "+localTime(c.value.generated_at_ms) : "";
    if (kind === "quotas" && c.value) {
      let latest = 0;
      for (const account of c.value.data.accounts) {
        if (Number.isSafeInteger(account.observed_at_ms)) latest = Math.max(latest, account.observed_at_ms);
      }
      ready = "Last quota read · "+localTime(latest);
    }
    const status=c.error?errorText(c.error):c.loading?"Refreshing…":kind==="asks"&&c.value?.data.setup==="not_set_up"?"Asks not enabled":c.value?.partial&&!omissions&&!paging?"Partial data · updated "+localTime(c.value.generated_at_ms):c.value?ready:s.layout==="unavailable"?"Herdr connection unavailable":"Not loaded";
    const quotaRead = kind === "quotas" && (c.error || c.loading) ? ready : "";
    const text=[name,status,c.error&&c.value?"Showing last known data":"",quotaRead,paging,omissions].filter(Boolean).join(" · ");
    return omissions||c.error==="too_large"?this.ui.warning(text):this.ui.label(text,true);
  }
  appearancePicker() {
    const t = this.ui.colors();
    return Popover.new("appearance").anchor("top_right").open(this.appearanceOpen)
      .on_open_change((open, cx) => { this.appearanceOpen = open; cx.notify(); })
      .trigger(div().px(12).py(7).rounded(5).border(1).border_color(t.border).bg(t.surface)
        .text_color(t.foreground).text_size(13).child(`Appearance · ${this.appearance} ▾`))
      .content(v_flex().w(150).p(6).gap(4).rounded(6).border(1).border_color(t.border).bg(t.chrome)
        .children(appearances.map(value => this.ui.button("appearance-" + value, value, (_event, cx) => {
          this.appearance = value;
          this.appearanceOpen = false;
          saveAppearance(value);
          cx.notify();
        }, value === this.appearance))));
  }
  toolbar(cx) {
    const t = this.ui.colors();
    return h_flex().gap(12).items_center().px(24).py(14).bg(t.chrome).border_b(1).border_color(t.border).children([
      div().font_semibold().text_size(20).child("aiq"),
      ...["asks","sessions","quotas"].map(tab=>this.ui.button("tab-"+tab,tab[0].toUpperCase()+tab.slice(1),(_e,cx)=>this.setTab(tab,cx),this.tab===tab)),
      div().flex_1(),
      this.appearancePicker(),
      this.ui.button("marks",this.marks?"Markdown symbols ✓":"Markdown symbols",(_e,cx)=>{this.marks=!this.marks;cx.notify();},this.marks),
      this.ui.button("refresh",this.conversation?"Refresh conversation":"Refresh",(_e,cx)=>this.conversation?this.openConversation(this.conversation.row,cx,{at:this.conversation.value?.messages.find(m=>m.anchor)?.at_ms||this.conversation.value?.messages[0]?.at_ms}):this.refresh(cx))
    ]);
  }
  render(cx) {
    const t=this.ui.colors();
    return v_flex().id("root").size_full().min_h(0).bg(t.surface).text_color(t.foreground).text_size(14).track_focus(this.handle)
      .on_size_change((size,cx)=>{if(size.width!==this.size.width||size.height!==this.size.height){this.size=size;cx.notify();}})
      .on_key_down((e,cx)=>{
        if(e.keystroke==="cmd-shift-m"){this.marks=!this.marks;cx.stop_propagation();cx.notify();}
        if(e.key==="escape" && this.selection.peek){this.selection.peek=null;this.context=null;cx.stop_propagation();cx.notify();}
      }).children([
        this.toolbar(cx),
        h_flex().px(24).py(8).gap(8).items_center().children([
          this.ui.label("Machines",true),...["all",...this.sources.keys()].map(key=>this.ui.button("machine-"+key,key==="all"?"All":key||"Here",(_e,cx)=>{this.machine=key;this.tablePage=0;this.reconcile(cx);this.refresh(cx);cx.notify();},this.machine===key))
        ]),
        h_flex().px(24).py(4).gap(18).flex_wrap().children(this.sourceList().map(s=>this.status(s,this.tab))),
        this.notice||this.undo.length?h_flex().px(24).py(8).gap(12).items_center().children([this.ui.label(this.notice||`${this.undo.length} dismissal${this.undo.length===1?"":"s"} can be undone.`),...(this.undo.length?[this.ui.button("undo","Undo",(_e,cx)=>this.dismiss(this.undo,cx,"restore"),false,this.busy)]:[])]):null,
        this.conversation?this.conversationView(cx):this.tab==="asks"?this.asksView(cx):this.tab==="sessions"?this.sessionsView(cx):this.quotasView()
      ].filter(Boolean));
  }
  asksView(cx) {
    const list=this.groupList(),row=this.current(),group=list.find(g=>g.key===this.selection.group),t=this.ui.colors();
    const sidebar = v_flex().size_full().min_w(0).min_h(0).bg(t.chrome).children([
        v_flex().p(16).gap(10).children([
          this.ui.input(this.filter).rounded(5),
          h_flex().gap(8).flex_wrap().children([this.ui.button("pending","Pending",(_e,cx)=>{this.section="pending";this.reconcile(cx);cx.notify();},this.section==="pending"),this.ui.button("done","Done / dismissed",(_e,cx)=>{this.section="done";this.reconcile(cx);cx.notify();},this.section==="done")]),
          this.ui.label(`${list.length} sessions · ${list.reduce((n,g)=>n+g.rows.length,0)} loaded asks`,true)
        ]),
        div().id("session-group-viewport").relative().flex_1().min_h(0).w_full().on_size_change((size,cx)=>{if(Math.abs(this.groupsHeight-size.height)>1||Math.abs(this.groupsWidth-size.width)>1){this.groupsHeight=size.height;this.groupsWidth=size.width;cx.notify();}}).children([
          v_virtual_list("session-groups",list.length,askCardHeight + 2 * askCardMargin,i=>list[i].key,range=>list.slice(range.start,range.end).map(g=>
            v_flex().id("group-"+g.key).w(Math.max(1,this.groupsWidth-16)).mx(8).my(askCardMargin).p(12).gap(4).h(askCardHeight).rounded(6).border(1).border_color(g.key===this.selection.group?t.accent:t.border).bg(g.key===this.selection.group?t.raised:t.surface).children([
              h_flex().h(20).flex_shrink_0().gap(8).justify_between().children([
                div().min_w(0).font_semibold().line_height("20px").truncate().child(g.workspace.split("/").filter(Boolean).pop()||"Unknown workspace"),
                this.ui.label(g.rows.length).line_height("20px").flex_shrink_0()
              ]),
              this.ui.label(g.name?g.name+" · "+g.provider:g.provider+" · "+g.rows[0].conversation_id.slice(0,8),true).line_height("20px").flex_shrink_0().truncate(),
              this.ui.label((g.machine??"Here")+" · "+localTime(g.updated),true).line_height("20px").flex_shrink_0().truncate(),
              this.ui.label(g.workspace,true).line_height("20px").flex_shrink_0().truncate()
            ]))).h(Math.max(1,this.groupsHeight)).w_full().track_scroll(this.groupsScroll).on_item_click((key,cx)=>{const group=list.find(g=>g.key===key);if(group)this.choose(group.rows[0],cx);}),
          Scrollbar.vertical("session-groups").absolute().inset_0()
        ]),
        !list.length?div().p(16).child("No matching asks in the loaded data."):null
    ].filter(Boolean));
    const detail = row && group
      ? this.askView(row,group,cx)
      : this.ui.panel("ask-empty").child(this.ui.label("Choose a session to review its asks.",true));
    const sidebarMax = Math.max(260, Math.min(640, this.size.width * 0.5));
    return h_resizable("asks-split").w_full().flex_1().min_h(0)
      .h(Math.max(300,this.size.height-150))
      .child(resizable_panel().size(320).size_range(260, sidebarMax).child(sidebar))
      .child(resizable_panel().size_range(300).child(detail));
  }
  askView(row,group,cx) {
    const index=group.rows.findIndex(r=>rowKey(r)===rowKey(row)),peeking=this.selection.peek===rowKey(row),data=this.detail?.value;
    const dismissable=group.rows.filter(r=>r.state==="open"||r.state==="probably-done");
    const restore=row.state==="dismissed"||row.state==="closed";
    const question=v_flex().h_full().flex_1().min_w(0).min_h(0).children([
      v_flex().px(24).py(16).gap(10).border_b(1).border_color(this.ui.colors().border).children([
        h_flex().gap(8).items_center().children([
          div().flex_1().min_w(0).font_semibold().truncate().child(group.name||group.workspace),
          this.ui.button("prev","←",(_e,cx)=>this.choose(group.rows[index-1],cx),false,index<=0),this.ui.label(`${index+1} / ${group.rows.length}`),this.ui.button("next","→",(_e,cx)=>this.choose(group.rows[index+1],cx),false,index>=group.rows.length-1)
        ]),
        ...(group.name?[this.ui.label(group.workspace,true)]:[]),
        this.ui.label(`${row.provider} · ${row.conversation_id.slice(0,8)} · ${row.machine??"Here"}`,true),
        ...(dismissable.length?[h_flex().gap(8).children([this.ui.button("batch",`Dismiss ${dismissable.length} session asks`,(_e,cx)=>this.dismiss(dismissable,cx),false,this.busy)])]:[])
      ]),
      this.ui.panel("question").child(v_flex().gap(16).children([
        this.ui.label(`First asked ${localTime(row.raised_at_ms)}${row.repeats?" · repeated "+row.repeats+" times":""} · Ask ${row.id}`,true),
        this.detail?.loading?this.ui.label("Loading full ask…",true):null,
        this.detail?.error?this.ui.label(errorText(this.detail.error),true):null,
        this.prose("ask-text-"+row.id,data?.latest.text??row.latest.preview,this.marks),
        (data?.latest.truncated || !data&&row.latest.truncated)?this.ui.warning("Ask excerpt · display limit reached."):null,
        h_flex().gap(8).flex_wrap().children([
          this.ui.button("dismiss",restore?"Restore":"Dismiss",(_e,cx)=>this.dismiss([row],cx,restore?"restore":"dismiss"),true,this.busy),
          ...(row.state==="probably-done"?[this.ui.button("reopen","Reopen ask",(_e,cx)=>this.dismiss([row],cx,"restore"),false,this.busy)]:[]),
          this.ui.button("peek",peeking?"Close context":"Peek at original context",(_e,cx)=>this.peek(row,cx),peeking),
          this.ui.button("copy","Copy ask",(_e,cx)=>cx.write_to_clipboard(data?.latest.text??row.latest.preview),false,!data),
          this.ui.button("terminal",row.terminal?.relation==="workspace_current"?"Current workspace terminal":"Go to terminal",(_e,cx)=>this.focusTerminal(row,cx),false,this.busy||!row.terminal)
        ]),
        !row.terminal?this.ui.label(terminalText(row.terminal_reason),true):null,
        this.ui.label("Reply in the session’s terminal.",true)
      ].filter(Boolean)))
    ]);
    return h_flex().h_full().items_stretch().flex_1().min_w(0).min_h(0).children([question,...(peeking?[this.contextView(row,cx)]:[])]);
  }
  contextView(row,cx) {
    const c=this.context,t=this.ui.colors();
    return v_flex().h_full().w("46%").min_w(0).min_h(0).bg(t.chrome).border_l(1).border_color(t.border).children([
      v_flex().px(24).py(16).gap(10).border_b(1).border_color(t.border).children([
        h_flex().gap(8).items_center().children([div().font_semibold().flex_1().child("Original context"),this.ui.button("refresh-context","Refresh",(_e,cx)=>{this.selection.peek=null;this.peek(row,cx);}),this.ui.button("close-context","Close",(_e,cx)=>{this.selection.peek=null;this.context=null;cx.notify();})]),
        this.ui.input(this.search).rounded(5),
        this.ui.label(c?.value?.placement?.confidence==="time+text"?"Matched ask highlighted below":c?.value?.placement?.confidence==="time"?"Located by timestamp; check the highlighted message":"Original turn not located",true),
        c?.omissions?this.ui.warning(c.omissions):null
      ]),
      c?.loading?this.ui.panel("context-loading").child("Loading conversation…"):c?.error?this.ui.panel("context-error").child(errorText(c.error)):this.messageList(c?.value,this.contextScroll,"context-messages"),
      c?.value?this.pageControls(c.value,{machine:row.machine,host:row.host,provider:row.provider,id:row.conversation_id},cx,true):null
    ].filter(Boolean));
  }
  messageList(data,scroll,id) {
    if(!data?.messages.length)return this.ui.panel(id).child(this.ui.label("No messages in this window.",true));
    const t=this.ui.colors();
    return v_flex().id(id).flex_1().min_h(0).overflow_y_scrollbar().track_scroll(scroll).p(24).gap(16).children(data.messages.map(m=>
      v_flex().id(id+"-"+m.key).p(16).gap(10).border(1).border_color(m.anchor||this.match===m.key?t.accent:t.border).rounded(6).bg(m.anchor||this.match===m.key?t.raised:t.surface).children([
        this.ui.label(`${m.source||m.role} · ${localTime(m.at_ms)}${m.anchor?" · matching ask":""}`,true),this.prose(id+"-text-"+m.key,m.text,this.marks),...(m.truncated?[this.ui.warning("Message excerpt · display limit reached.")]:[])
      ])));
  }
  pageControls(data,row,cx,context=false) {
    return h_flex().px(24).py(12).gap(8).children([
      this.ui.button("earlier","Earlier messages",(_e,cx)=>this.openConversation(row,cx,{before:data.cursor.before}),false,!data.cursor?.before),
      this.ui.button("later","Later messages",(_e,cx)=>this.openConversation(row,cx,{after:data.cursor.after}),false,!data.cursor?.after),
      ...(context?[this.ui.button("full","Full conversation",(_e,cx)=>this.openConversation(row,cx,data.messages?.some(m=>m.anchor)?{anchor:data.messages.find(m=>m.anchor).key}:{at:data.placement?.anchor_ms}))]:[])
    ]);
  }
  conversationView(cx) {
    const c=this.conversation;
    return v_flex().h(Math.max(300,this.size.height-150)).flex_1().min_h(0).children([
      h_flex().px(24).py(14).gap(12).items_center().children([
        this.ui.button("back","← Back",(_e,cx)=>{this.conversation=null;cx.notify();}),this.ui.label(c.row.name||c.row.cwd||c.row.id),div().flex_1(),this.ui.input(this.search).w(300)
      ]),
      c.omissions?div().px(24).child(this.ui.warning(c.omissions)):null,
      c.loading?this.ui.panel("conversation-loading").child("Loading conversation…"):c.error?this.ui.panel("conversation-error").child(errorText(c.error)):this.messageList(c.value,this.scroll,"conversation"),
      c.value?this.pageControls(c.value,c.row,cx):null
    ].filter(Boolean));
  }
  sessionsView(cx) {
    let rows=[];const seen=new Set(),q=this.query.toLowerCase();
    for(const s of this.sourceList()){const e=s.sessions?.value;if(!e||seen.has(e.host.id))continue;seen.add(e.host.id);rows.push(...e.data.rows.map(r=>({...r,machine:s.machine,host:e.host.id})));}
    rows=rows.filter(r=>[r.name,r.cwd,r.provider,r.machine??"Here",r.live].join(" ").toLowerCase().includes(q));
    rows=rows.filter(r=>(this.workers||!r.worker)&&(!this.hasAsks||r.pending_asks_known&&r.pending_asks>0)&&(!this.liveOnly||r.live==="live"));
    const direction=this.desc?-1:1,key=this.sort;rows.sort((a,b)=>direction*(typeof a[key]==="number"?a[key]-b[key]:String(a[key]??"").localeCompare(String(b[key]??""))));
    const keyFor=r=>[r.host,r.provider,r.kind,r.id||r.lease_id||r.cwd].join("\u001f");
    const selected=rows.find(r=>keyFor(r)===this.tableSelected);
    const cols=[["name","Session"],["cwd","Workspace"],["machine","Machine"],["provider","Provider"],["pending_asks","Asks"],["updated_ms","Last activity"],["live","Status"],["terminal","Terminal"]];
    const widths=["22%","26%","8%","8%","6%","17%","7%","6%"];
    return v_flex().h(Math.max(300,this.size.height-150)).flex_1().min_h(0).px(24).py(16).gap(14).children([
      h_flex().gap(8).items_center().flex_wrap().children([
        this.ui.input(this.filter).w(320),
        ...[["hasAsks","Has asks"],["liveOnly","Live only"],["workers","Include workers"]].map(([key,text])=>this.ui.button("filter-"+key,text,(_e,cx)=>{this[key]=!this[key];this.tablePage=0;cx.notify();},this[key])),
        this.ui.label(`${rows.length} loaded rows`,true),div().flex_1(),
        ...this.sourceList().filter(s=>s.sessions?.value?.data.window.cursor).map(s=>this.ui.button("older-"+machineKey(s.machine),"Older · "+(s.machine??"Here"),(_e,cx)=>this.load(s,"sessions",cx,{before:s.sessions.value.data.window.cursor},true),false,s.sessions.loading))
      ]),
      h_flex().gap(8).items_center().children([
        this.ui.button("read-selected","Read conversation",(_e,cx)=>this.openConversation(selected,cx),false,!selected?.id),
        this.ui.button("asks-selected","Review asks",(_e,cx)=>this.showSessionAsks(selected,cx),false,!selected?.pending_asks_known||selected.pending_asks<1),
        this.ui.button("terminal-selected","Go to terminal",(_e,cx)=>this.focusTerminal(selected,cx),false,this.busy||(!selected?.id&&!selected?.lease_id)||!["live","open"].includes(selected.live)&&!selected.terminal),
        this.ui.label("Select a row · double-click to read",true)
      ]),
      div().id("sessions-table-scroll").min_h(0).flex_1().child(
        Table.new("sessions-grid").flex().flex_col().flex_1().min_h(0).h_full().w_full().accessibility_label("Sessions across machines").row_count(rows.length+1).column_count(cols.length).children([
          TableHeader.new("headers").flex().flex_col().w_full().child(TableRow.new("header-row",1).flex().flex_row().items_center().w_full().children(cols.map(([key,title],i)=>TableHead.new("head-"+key,i+1).w(widths[i]).min_w(0).flex_shrink_0().p(6).child(key==="terminal"?this.ui.label(title):this.ui.button("sort-"+key,title+(this.sort===key?(this.desc?" ↓":" ↑"):""),(_e,cx)=>{this.desc=this.sort===key?!this.desc:true;this.sort=key;this.tablePage=0;cx.notify();}).px(6).w_full().truncate())))),
          TableBody.new("session-body").flex().flex_col().flex_1().min_h(0).w_full().child(
            div().id("session-grid-viewport").relative().flex_1().min_h(0).min_w(0).w_full().track_focus(this.gridFocus).on_size_change((size,cx)=>{if(Math.abs(this.gridHeight-size.height)>1||Math.abs(this.gridWidth-size.width)>1){this.gridHeight=size.height;this.gridWidth=size.width;cx.notify();}}).on_key_down((event,cx)=>{
              const index=rows.findIndex(r=>keyFor(r)===this.tableSelected);
              if(event.keystroke==="up"||event.keystroke==="down"){const next=Math.max(0,Math.min(rows.length-1,index+(event.keystroke==="down"?1:-1)));if(rows[next]){this.tableSelected=keyFor(rows[next]);this.gridScroll.scroll_to_item(next,"center");cx.stop_propagation();cx.notify();}}
              const current=rows.find(r=>keyFor(r)===this.tableSelected);if(event.keystroke==="enter"&&current?.id){this.openConversation(current,cx);cx.stop_propagation();}
            }).children([
            v_virtual_list("session-rows",rows.length,40,i=>keyFor(rows[i]),range=>rows.slice(range.start,range.end).map((r,i)=>TableRow.new("session-"+keyFor(r),range.start+i+2).flex().flex_row().items_center().h(40).w(Math.max(1,this.gridWidth)).bg(keyFor(r)===this.tableSelected?this.ui.colors().raised:this.ui.colors().surface).children(cols.map(([key],j)=>{
            let value=key==="name"?(r.name||r.id.slice(0,8)||"Workspace history"):key==="machine"?(r.machine??"Here"):key==="pending_asks"?(r.pending_asks_known?String(r.pending_asks):"Unknown"):key==="updated_ms"?localTime(r.updated_ms):r[key];
            if(key==="name"&&!r.id)value="Recorded workspace";
            if(key==="terminal")value=r.terminal?"Linked":"—";
            const child=this.ui.label(value).truncate();
            return TableCell.new("cell-"+i+"-"+j,j+1).w(widths[j]).min_w(0).flex_shrink_0().p(6).border_b(1).border_color(this.ui.colors().border).child(child);
          })))).h(Math.max(1,this.gridHeight)).w_full().track_scroll(this.gridScroll).on_item_click((key,cx)=>{
            const row=rows.find(r=>keyFor(r)===key);if(!row)return;
            const now=Date.now(),double=this.lastRowClick?.key===key&&now-this.lastRowClick.at>=0&&now-this.lastRowClick.at<350;
            this.tableSelected=key;this.lastRowClick={key,at:now};this.gridFocus.focus();if(double&&row.id)this.openConversation(row,cx);cx.notify();
          }),
            Scrollbar.vertical("session-rows").absolute().inset_0()
          ]))
        ]))
    ]);
  }
  quotaCard(account) {
    const t = this.ui.colors();
    const identity = [account.plan, account.label || account.identity].filter(Boolean).join(" · ");
    const resets = quotaResets(account.windows);
    const grant = quotaResetGrant(account);
    const renewals = quotaRenewals(account);
    return v_flex().min_w(0).p(12).gap(6).rounded(6).border(1).border_color(t.border).children([
      h_flex().justify_between().items_start().gap(10).children([
        h_flex().min_w(0).gap(7).flex_wrap().children([
          div().font_semibold().child(account.id.startsWith(account.provider+"/") ? account.id : account.provider+"/"+account.id),
          identity ? this.ui.label(identity,true) : null
        ].filter(Boolean)),
        this.ui.label(account.rank && account.score !== null
          ? `#${account.rank} · ${Number(account.score).toFixed(1)}/h` : "Unranked",true).flex_shrink_0()
      ]),
      ...account.windows.map(w => {
        const name = w.label || w.key;
        const used = w.used_pct === null || w.used_pct < 0 ? null : w.used_pct;
        return h_flex().items_center().gap(8).children([
          this.ui.label(name,w.binding===false).min_w(0).w("33%").truncate().tooltip(name),
          div().flex_1().min_w(0).h(6).rounded(3).bg(t.raised).child(
            div().h(6).rounded(3).w(Math.min(100,used??0)+"%").bg(used>=95?t.ansi1:used>=80?t.ansi3:t.ansi2)
          ),
          this.ui.label(used===null?"Unknown":used.toFixed(0)+"%").w(60).text_right().flex_shrink_0()
        ]);
      }),
      resets || grant ? h_flex().w_full().min_w(0).gap(12).flex_wrap().children([
        resets ? this.ui.label(resets,true).min_w(0).max_w_full().whitespace_normal() : null,
        grant ? this.ui.label(grant,true).min_w(0).max_w_full().whitespace_normal()
          .text_color(account.reset_credits>0?t.ansi2:t.muted) : null
      ].filter(Boolean)) : null,
      !account.eligible && !renewals.some(hint=>hint.kind==="login")
        ? this.ui.label(account.ineligible || account.reason || "Currently ineligible",true) : null,
      ...account.leases.map(l => {
        const text = `${l.mode} · ${l.workspace || l.cwd}`;
        return this.ui.label(text,true).min_w(0).truncate().tooltip(text);
      }),
      ...renewals.map(hint => h_flex().items_baseline().gap(8).flex_wrap().children([
        this.ui.label(hint.text).text_color(t.ansi1).font_semibold(),
        hint.command ? this.ui.label(hint.command).font_family("Menlo") : null
      ].filter(Boolean))),
      account.poll_error && !renewals.some(hint=>hint.kind==="authorize") && account.ineligible!=="authentication required"
        ? this.ui.label("Latest quota read failed.",true) : null
    ].filter(Boolean));
  }
  quotasView() {
    const t=this.ui.colors();
    return v_flex().id("quotas-scroll").h(Math.max(300,this.size.height-150)).flex_1().min_h(0).overflow_y_scrollbar().px(24).py(10).gap(18).children(this.sourceList().map(s=>{
      const e=s.quotas?.value;
      return v_flex().gap(8).children([
        div().font_semibold().text_size(18).child(s.machine??"Here"),
        ...(e?[h_flex().gap(16).items_start().children(["agy","claude","codex",...(e.data.accounts.some(a=>a.provider==="copilot")?["copilot"]:[])].map(provider=>v_flex().flex_1().min_w(0).gap(8).children([
          div().text_color(t.muted).font_semibold().child(provider.toUpperCase()),
          ...e.data.accounts.filter(a=>a.provider===provider).map(account=>this.quotaCard(account))
        ])))]:[this.ui.label("No quota snapshot available.",true)])
      ]);
    }));
  }
}
