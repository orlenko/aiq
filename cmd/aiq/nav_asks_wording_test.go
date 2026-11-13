package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

// Invented test allowances only; the production entry point has no defaults.
func asksWordOptions() navAsksWordingOptions {
	return navAsksWordingOptions{width: 10, inputBytes: 10000, renderCells: 10000000, arrayEntries: 10000, stringUnits: 10000}
}
func asksWordText(lines []navConversationLine) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		var text strings.Builder
		for _, span := range line.Spans {
			text.WriteString(span.Text)
		}
		out = append(out, text.String())
	}
	return out
}
func asksWordPlain(text string) []webMarkdownLine {
	return []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: text}}}}
}

func TestNavAsksWordingLiteralRecipes(t *testing.T) {
	for _, tc := range []struct{ raw, copy string }{
		{"", ">\n\n"}, {"one\n\ntwo\n", "> one\n>\n> two\n\n"},
		{" \tword  \n\n", ">  \tword\n\n"}, {"a\t \n", "> a\t\n\n"},
		{"a\x00\x1b\x7f\u202eb\u2028c\u2029d\u2066\xff", "> ab\n> c\n> d�\n\n"},
	} {
		out, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: tc.raw, LastQuote: tc.raw}, asksWordOptions())
		if err != nil || out.First.Copy != tc.copy || out.Latest.Copy != tc.copy {
			t.Fatalf("raw %q: %#v %v", tc.raw, out, err)
		}
	}
	a := state.Ask{FirstQuote: "  [label](https://example.invalid/a)  ", LastQuote: "latest"}
	out, err := projectNavAsksWording(context.Background(), a, asksWordOptions())
	if err != nil || !reflect.DeepEqual(asksWordText(out.First.Lines), []string{"label"}) || out.First.Copy != ">   [label](https://example.invalid/a)\n\n" || out.Latest.Copy != "> latest\n\n" {
		t.Fatalf("separate paint/copy: %#v %v", out, err)
	}
	for _, raw := range []string{"```\nx" + strings.Repeat(" ", 19) + "\n```", "```\nx\n \n```", "```\n```", ">", "---"} {
		out, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: raw}, asksWordOptions())
		if err != nil {
			t.Fatal(err)
		}
		switch raw {
		case "```\n```":
			if len(out.First.Lines) != 0 || out.First.Lines == nil {
				t.Fatal("empty fence must be []")
			}
		case ">", "---":
			if len(out.First.Lines) != 1 || !out.First.Lines[0].Spans[0].Decorative {
				t.Fatal("decoration pruned")
			}
		default:
			want := "x"
			if strings.Contains(raw, strings.Repeat(" ", 19)) {
				want += strings.Repeat(" ", 19)
			}
			if !reflect.DeepEqual(asksWordText(out.First.Lines), []string{want}) {
				t.Fatalf("logical suffix: %#v", out.First.Lines)
			}
		}
	}
}

func TestNavAsksWordingLiteralAdmission(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		width       int
		work, units uint64
	}{{"", 1, 22, 6}, {"", 20, 42, 6}, {"z", 1, 35, 13}, {"😀", 1, 86, 15}} {
		o := asksWordOptions()
		o.width, o.renderCells, o.stringUnits = tc.width, tc.work, tc.units
		out, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: tc.raw}, o)
		if err != nil || out == nil {
			t.Fatalf("exact boundary %+v: %v", tc, err)
		}
		o.renderCells--
		calls := 0
		render := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
			calls++
			return webMarkdownLinesChecked(s, w, m)
		}
		out, err = projectNavAsksWordingWith(context.Background(), state.Ask{FirstQuote: tc.raw}, o, render, navAsksWordingValue)
		if out != nil || err != errNavAsksWordingTooLarge || calls != 0 {
			t.Fatalf("work admission: %#v %v calls%d", out, err, calls)
		}
		o.renderCells++
		o.stringUnits--
		if out, err = projectNavAsksWording(context.Background(), state.Ask{FirstQuote: tc.raw}, o); out != nil || err != errNavAsksWordingTooLarge {
			t.Fatal("text boundary")
		}
	}
	o := asksWordOptions()
	o.inputBytes, o.renderCells, o.arrayEntries, o.stringUnits = 1, 35, 2, 13
	if _, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: "z"}, o); err != nil {
		t.Fatal(err)
	}
	o.arrayEntries = 1
	if v, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: "z"}, o); v != nil || err != errNavAsksWordingTooLarge {
		t.Fatal("entries boundary")
	}
	for _, mutate := range []func(*navAsksWordingOptions){func(o *navAsksWordingOptions) { o.inputBytes = 1 }, func(o *navAsksWordingOptions) { o.renderCells = 47 }} {
		o := asksWordOptions()
		mutate(&o)
		calls := 0
		render := func(string, int, bool) ([]webMarkdownLine, bool) { calls++; return nil, true }
		v, err := projectNavAsksWordingWith(context.Background(), state.Ask{FirstQuote: "a", LastQuote: "a"}, o, render, navAsksWordingValue)
		if v != nil || err != errNavAsksWordingTooLarge || calls != 0 {
			t.Fatal("second quote must pre-admit before first transform")
		}
	}
	o = asksWordOptions()
	o.width = int(^uint(0) >> 1)
	o.renderCells = math.MaxUint64
	if int64(o.width) > navConversationSafeInteger {
		if v, err := projectNavAsksWording(context.Background(), state.Ask{}, o); v != nil || err != errNavAsksWordingTooLarge {
			t.Fatal("aggregate overflow admitted")
		}
	}
	remaining := uint64(64)
	if navAsksProduct(&remaining, math.MaxUint64, 2) || remaining != 64 || !navAsksProduct(&remaining, 8, 8) || remaining != 0 {
		t.Fatal("checked product overflow or boundary")
	}
}

func TestNavAsksWordingConfigurationAndCounts(t *testing.T) {
	for _, mutate := range []func(*navAsksWordingOptions){
		func(o *navAsksWordingOptions) { o.width = 0 }, func(o *navAsksWordingOptions) { o.width = -1 },
		func(o *navAsksWordingOptions) { o.inputBytes = 0 }, func(o *navAsksWordingOptions) { o.renderCells = 0 },
		func(o *navAsksWordingOptions) { o.arrayEntries = 0 }, func(o *navAsksWordingOptions) { o.stringUnits = 0 },
	} {
		o := asksWordOptions()
		mutate(&o)
		if v, err := projectNavAsksWording(context.Background(), state.Ask{}, o); v != nil || err != errNavAsksWordingInvalid {
			t.Fatal("invalid options accepted")
		}
	}
	if v, err := projectNavAsksWording(nil, state.Ask{}, asksWordOptions()); v != nil || err != errNavAsksWordingInvalid {
		t.Fatal("nil context")
	}
	if v, err := projectNavAsksWordingWith(context.Background(), state.Ask{}, asksWordOptions(), nil, navAsksWordingValue); v != nil || err != errNavAsksWordingInvalid {
		t.Fatal("nil renderer")
	}
	if v, err := projectNavAsksWordingWith(context.Background(), state.Ask{}, asksWordOptions(), webMarkdownLinesChecked, nil); v != nil || err != errNavAsksWordingInvalid {
		t.Fatal("nil converter")
	}
	for _, count := range []int{0, 2, -1, int(^uint(0) >> 1)} {
		render := func(string, int, bool) ([]webMarkdownLine, bool) {
			return []webMarkdownLine{{kind: "table", tableRows: count, spans: []webMarkdownSpan{{text: "cell"}}}}, true
		}
		v, err := projectNavAsksWordingWith(context.Background(), state.Ask{FirstQuote: "a"}, asksWordOptions(), render, navAsksWordingValue)
		valid := navConversationNumber(count)
		if valid && (err != nil || v.First.Lines[0].TableRows != count) || !valid && (v != nil || !errors.Is(err, diagnostic.Base(diagnostic.Unshowable))) {
			t.Fatalf("count%d: %#v %v", count, v, err)
		}
	}
}

func TestNavAsksWordingRaggedTableWork(t *testing.T) {
	a := state.Ask{FirstQuote: "a|b|c\n---|---|---\nx|y\nx|y"}
	o := asksWordOptions()
	o.inputBytes, o.renderCells = 25, 300
	calls := 0
	render := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
		calls++
		return webMarkdownLinesChecked(s, w, m)
	}
	if v, err := projectNavAsksWordingWith(context.Background(), a, o, render, navAsksWordingValue); v != nil || err != errNavAsksWordingTooLarge || calls != 0 {
		t.Fatal("quadratic dimension not pre-admitted")
	}
	o.renderCells = 947
	v, err := projectNavAsksWordingWith(context.Background(), a, o, render, navAsksWordingValue)
	if err != nil || calls != 1 || !reflect.DeepEqual(asksWordText(v.First.Lines), []string{"a: x", "b: y", "c:", "", "a: x", "b: y", "c:"}) || v.First.Copy != "> a|b|c\n> ---|---|---\n> x|y\n> x|y\n\n" {
		t.Fatalf("admitted fallback: %#v %v", v, err)
	}
}

func TestNavAsksWordingChargesDiscardedLines(t *testing.T) {
	a := state.Ask{FirstQuote: "z"}
	o := asksWordOptions()
	o.arrayEntries, o.stringUnits = 5, 24
	lines := append(asksWordPlain("z"), webMarkdownLine{kind: "code", spans: []webMarkdownSpan{{text: "   ", classes: []string{"code"}}}})
	render := func(string, int, bool) ([]webMarkdownLine, bool) { return lines, true }
	v, err := projectNavAsksWordingWith(context.Background(), a, o, render, navAsksWordingValue)
	if err != nil || len(v.First.Lines) != 1 {
		t.Fatalf("tail prune: %#v %v", v, err)
	}
	for _, mutate := range []func(*navAsksWordingOptions){func(o *navAsksWordingOptions) { o.arrayEntries-- }, func(o *navAsksWordingOptions) { o.stringUnits-- }} {
		x := o
		mutate(&x)
		if v, err = projectNavAsksWordingWith(context.Background(), a, x, render, navAsksWordingValue); v != nil || err != errNavAsksWordingTooLarge {
			t.Fatal("discarded tail escaped quota")
		}
	}
	for _, mutate := range []func(*webMarkdownLine){func(l *webMarkdownLine) { l.kind = "future" }, func(l *webMarkdownLine) { l.tableRows = 1 }, func(l *webMarkdownLine) { l.spans[0].classes[0] = "future" }, func(l *webMarkdownLine) { l.spans[0].text = "\xff" }} {
		x := webMarkdownLine{kind: "code", spans: []webMarkdownSpan{{text: "   ", classes: []string{"code"}}}}
		mutate(&x)
		bad := func(string, int, bool) ([]webMarkdownLine, bool) { return append(asksWordPlain("z"), x), true }
		if v, err := projectNavAsksWordingWith(context.Background(), a, asksWordOptions(), bad, navAsksWordingValue); v != nil || !errors.Is(err, diagnostic.Base(diagnostic.Unshowable)) {
			t.Fatal("invalid tail hidden by pruning")
		}
	}
}

func TestNavAsksWordingWhitespaceLabelIsNotSkipped(t *testing.T) {
	o := asksWordOptions()
	o.stringUnits, o.arrayEntries = 34, 2
	calls := 0
	render := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
		calls++
		if s != " " || m {
			t.Fatal("prepared whitespace/mode changed")
		}
		return webMarkdownLinesChecked(s, w, m)
	}
	a := state.Ask{FirstQuote: "[ ](https://e.invalid)"}
	v, err := projectNavAsksWordingWith(context.Background(), a, o, render, navAsksWordingValue)
	if err != nil || calls != 1 || len(v.First.Lines) != 0 || v.First.Lines == nil || v.First.Copy != "> [ ](https://e.invalid)\n\n" {
		t.Fatalf("whitespace label: %#v %v", v, err)
	}
	o.stringUnits--
	if v, err = projectNavAsksWordingWith(context.Background(), a, o, render, navAsksWordingValue); v != nil || err != errNavAsksWordingTooLarge {
		t.Fatal("whitespace primitive not charged")
	}
}

func TestNavAsksWordingCancellationAndCoverage(t *testing.T) {
	a := state.Ask{FirstQuote: "a", LastQuote: "b"}
	for _, when := range []string{"entry", "first", "second", "convert"} {
		ctx, cancel := context.WithCancel(context.Background())
		calls, converts := 0, 0
		if when == "entry" {
			cancel()
		}
		render := func(string, int, bool) ([]webMarkdownLine, bool) {
			calls++
			if when == "first" && calls == 1 || when == "second" && calls == 2 {
				cancel()
			}
			return asksWordPlain("x"), false
		}
		if when == "second" || when == "convert" {
			render = func(string, int, bool) ([]webMarkdownLine, bool) {
				calls++
				if when == "second" && calls == 2 {
					cancel()
					return nil, false
				}
				return asksWordPlain("x"), true
			}
		}
		convert := func(d [2]navAsksWordingDraft) *navAsksWordingPair {
			converts++
			v := navAsksWordingValue(d)
			cancel()
			return v
		}
		v, err := projectNavAsksWordingWith(ctx, a, asksWordOptions(), render, convert)
		cancel()
		if v != nil || err != context.Canceled {
			t.Fatalf("%s: %#v %v", when, v, err)
		}
		if when == "entry" && calls != 0 || when == "convert" && converts != 1 {
			t.Fatal("wrong cancellation boundary")
		}
	}
	calls, converts := 0, 0
	render := func(string, int, bool) ([]webMarkdownLine, bool) { calls++; return asksWordPlain("x"), calls == 1 }
	convert := func(d [2]navAsksWordingDraft) *navAsksWordingPair { converts++; return navAsksWordingValue(d) }
	if v, err := projectNavAsksWordingWith(context.Background(), a, asksWordOptions(), render, convert); v != nil || !errors.Is(err, diagnostic.Base(diagnostic.Unshowable)) || calls != 2 || converts != 0 {
		t.Fatal("late coverage cloned/published a partial pair")
	}
}

type asksWordHostile struct{ calls *int }

func (h asksWordHostile) Error() string  { (*h.calls)++; return "invented private cause" }
func (h asksWordHostile) String() string { (*h.calls)++; return "invented private cause" }
func (h asksWordHostile) Unwrap() error  { (*h.calls)++; return nil }
func (h asksWordHostile) Is(error) bool  { (*h.calls)++; return false }
func (h asksWordHostile) As(any) bool    { (*h.calls)++; return false }

func TestNavAsksWordingPanicFirstOrigin(t *testing.T) {
	for _, prior := range []string{"none", "known", "unsupported"} {
		var seed diagnostic.Origin
		if prior != "none" {
			func() {
				defer func() { seed = diagnostic.Capture(recover()) }()
				if prior == "unsupported" {
					regexp.MustCompile("[")
				}
				panic("invented first origin")
			}()
		}
		if prior == "unsupported" {
			if _, _, known := seed.Location(); known || !seed.Present() {
				t.Fatal("fixture must have a non-allowlisted origin")
			}
		}
		for _, at := range []int{1, 2} {
			faults := &diagnostic.Latch{}
			faults.Note(seed)
			o := asksWordOptions()
			o.faults = faults
			ctx, cancel := context.WithCancel(context.Background())
			calls, methods := 0, 0
			render := func(string, int, bool) ([]webMarkdownLine, bool) {
				calls++
				if calls == at {
					cancel()
					panic(asksWordHostile{&methods})
				}
				return asksWordPlain("x"), true
			}
			v, err := projectNavAsksWordingWith(ctx, state.Ask{FirstQuote: "a", LastQuote: "b"}, o, render, navAsksWordingValue)
			cancel()
			origin := diagnostic.FromError(err)
			if v != nil || !errors.Is(err, diagnostic.Base(diagnostic.Unshowable)) || !origin.Present() || methods != 0 || strings.Contains(err.Error(), "private") {
				t.Fatalf("panic result %#v %v methods%d", v, err, methods)
			}
			if seed.Present() && (origin != seed || faults.Origin() != seed) {
				t.Fatal("first origin replaced")
			}
			if !seed.Present() {
				if file, _, known := origin.Location(); !known || file != "cmd/aiq/nav_asks_wording_test.go" {
					t.Fatal("actual literal panic leaf not captured")
				}
			}
		}
	}
}

func TestNavAsksWordingOwnershipAndPalette(t *testing.T) {
	backing := strings.Repeat(".", 65536) + "code" + "z"
	kind, text := backing[65536:65540], backing[65540:]
	lines := []webMarkdownLine{{kind: kind, spans: []webMarkdownSpan{{text: text, classes: []string{kind, kind}}}}}
	render := func(string, int, bool) ([]webMarkdownLine, bool) { return lines, true }
	v, err := projectNavAsksWordingWith(context.Background(), state.Ask{FirstQuote: "z", LastQuote: "z"}, asksWordOptions(), render, navAsksWordingValue)
	if err != nil {
		t.Fatal(err)
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(backing)))
	end := start + uintptr(len(backing))
	for _, s := range []string{v.First.Lines[0].Kind, v.First.Lines[0].Spans[0].Text, v.First.Lines[0].Spans[0].Classes[0]} {
		ptr := uintptr(unsafe.Pointer(unsafe.StringData(s)))
		if ptr >= start && ptr < end {
			t.Fatal("owned string retained borrowed backing")
		}
	}
	lines[0].spans[0].classes[0] = "changed"
	lines[0].spans[0].text = "changed"
	v.First.Lines[0].Spans[0].Classes[1] = "result"
	if v.Latest.Lines[0].Spans[0].Text != "z" || !reflect.DeepEqual(v.Latest.Lines[0].Spans[0].Classes, []string{"code", "code"}) {
		t.Fatal("nested variants alias")
	}
	saved := pal
	defer func() { pal = saved }()
	pal = kitMonoPalette
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: "**bold**"}, asksWordOptions())
			if err != nil || !reflect.DeepEqual(out.First.Lines[0].Spans[0].Classes, []string{"b"}) {
				t.Errorf("immutable rendering: %#v %v", out, err)
			}
		}()
	}
	wg.Wait()
	if pal != kitMonoPalette {
		t.Fatal("global palette changed")
	}
}

func TestNavAsksWordingJSONConsumer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required in CI")
		}
		t.Skip("node missing: consumer compatibility NOT RUN")
	}
	pair, err := projectNavAsksWording(context.Background(), state.Ask{FirstQuote: "one\n\ntwo\n", LastQuote: ""}, asksWordOptions())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(pair)
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs("../../internal/daemon/ui/asks_data.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `import {readFileSync} from 'node:fs';import assert from 'node:assert/strict';
const {captureAsks}=await import(process.argv[1]);const pair=JSON.parse(readFileSync(0,'utf8'));
const at={ms:0,known:false};const item={id:'1',provider:'unavailable',session_id:'none',workspace:'/fixture',state:'open',raised_at:at,state_at:at,repeats:0,state_reason:'',state_text:'',refs:[],...pair};
const scope={mode:'all',dir:''};const sections=['asks','warnings','other_open','probably_done','closed_dismissed'].map((key,i)=>({key,items:i===0?[item]:[]}));
const out=captureAsks({scope,sections},scope,{arrayEntries:1000,stringUnits:10000,rows:1});
assert.equal(out.sections[0].items[0].first.copy,'> one\n>\n> two\n\n');assert.deepEqual(pair.latest,{lines:[],copy:'>\n\n'});assert.equal(out.sections[0].items[0].selectable,false);console.log('consumer pair OK');`
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, (&url.URL{Scheme: "file", Path: module}).String())
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(string(data))
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "consumer pair OK\n" {
		t.Fatalf("consumer fixture: %s %v", output, err)
	}
}
