package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func foldFixture(which int) (string, []convoCut) {
	var text strings.Builder
	var cuts []convoCut
	add := func(key, kind, raw, paint string) {
		text.WriteString(paint)
		cuts = append(cuts, convoCut{end: text.Len(), key: key, kind: kind, text: raw})
	}
	switch which {
	case 0:
		add("turn-a", "you", "hello", "\nYOU\nhello\n")
		add("turn-a", "answer", "body\n", "\n\x1b[31mANSWER\x1b[0m\nbody\n")
		add("turn-a", "more", "tail\n", "\nTAIL\n")
		add("turn-a", "part", "invisible\u2028\u2029", "")
		add("turn-a", "none", "ignored raw", "\n(no reply)\n")
		add("turn-a", "answer", "body", "\nANSWER\nbody\n")
		add("turn-a", "more", "", "\n\n")
	case 1:
		add("", "part", "ignored initial part", "discarded paint\n")
		add("", "more", "initial more", "\nMORE\n")
		add("", "part", "", "")
		add("", "none", "ignored raw", "\nNONE\n")
		add("", "you", "tab\t日本語", "\n\t日本語\n")
		add("", "answer", "x\x1b]52;c;fixture\a", "\n\x1b[32mstyled wide 日本語 row\x1b[0m\n")
	case 2:
		add("repeated", "none", "", "\nNONE\n")
		add("repeated", "more", "終\u2028\u2029", "\n終\n")
		add("repeated", "answer", "same", "\nSAME\n")
		add("repeated", "part", "first tail", "")
		add("repeated", "answer", "same", "\nSAME\n")
		add("repeated", "part", "different tail", "")
	case 3:
		add("", "answer", "", "\n\n\x1b[31m\x1b[0m\n\n")
		add("", "more", "\u2028", "\n\n")
		add("", "more", "", "")
		add("", "part", "", "")
		add("", "none", "", "\n")
	}
	// An unused backing element detects accidental reuse of caller capacity.
	cuts = append(cuts, convoCut{key: "spare-cut", text: "sentinel"})
	return text.String(), cuts[:len(cuts)-1]
}

func foldScreenBytes(c *convoScreen) []byte {
	type row struct {
		Text    string
		Message int
	}
	type message struct {
		ID, Kind, Raw string
		From, To      int
	}
	var out struct {
		Rows     []row
		Messages []message
	}
	for _, r := range c.rows {
		out.Rows = append(out.Rows, row{r.text, r.msg})
	}
	for _, m := range c.msgs {
		out.Messages = append(out.Messages, message{m.id, m.kind, m.raw, m.from, m.to})
	}
	data, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return data
}

func fragmentFixture(n int) (string, []convoCut) {
	var paint strings.Builder
	var cuts []convoCut
	paint.WriteString("\nROOT\n")
	cuts = append(cuts, convoCut{end: paint.Len(), key: "k", kind: "answer", text: "root"})
	for i := 0; i < n; i++ {
		paint.WriteString("\nxx\n")
		cuts = append(cuts, convoCut{end: paint.Len(), key: "k", kind: "more", text: strings.Repeat("x", 32)})
	}
	return paint.String(), cuts
}

var foldBenchSink *convoScreen

func BenchmarkConvoCutFragments(b *testing.B) {
	for _, n := range []int{1000, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			text, cuts := fragmentFixture(n)
			want := "root" + strings.Repeat("\n\n"+strings.Repeat("x", 32), n)
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := &convoScreen{}
				c.cut(0, text, cuts, 44)
				foldBenchSink = c
			}
			b.StopTimer()
			if len(foldBenchSink.msgs) != 1 || foldBenchSink.msgs[0].raw != want {
				b.Fatal("fragment copy bytes differ")
			}
			b.ReportMetric(float64(len(want)), "raw-bytes")
			b.Logf("raw SHA256 %x", sha256.Sum256([]byte(want)))
		})
	}
}

// Digests were captured from the old terminal loop before extraction. Each
// covers full styled rows, ownership, raw copy text, ranges and exact IDs over
// two calls into the same populated screen.
func TestConvoFoldPreservesTerminalProjection(t *testing.T) {
	cases := []struct {
		which, part, width int
		digest             string
	}{
		{0, 0, 8, "4d42ded1823c77d9fa51f7943d2a534cdba366f69e5c6b78a4debca28a8b299c"},
		{0, 0, 17, "c22322714ed98ae482044e3ce40c06007bab143a56038a73f8d84e454ed1169d"},
		{0, 0, 44, "eff8d37a89742f49665688dc8494f9f8bb86ab732f2a85d7e3028d2b67a33bd7"},
		{0, 3, 8, "1d0b24e799a2be0c1caa7fd1e80903905147237ecebc1edb20045e2ecce03465"},
		{0, 3, 17, "a697a84e3fa20457d3773be9a785a4b0515ec87abd912672d31c4b448de7f98a"},
		{0, 3, 44, "bb3771c3ef1867db40039c9e0b5c706df9a190af4d3ea77d32799f590a69b75d"},
		{1, 0, 8, "2ba6385903d10c0b9db7688d2d17d792e80aa867886895ac22ad2276acc7981b"},
		{1, 0, 17, "6e5851cc4d2a07138ac3abc2e87c1cd6af9b6676fdc002581982586a12bf816f"},
		{1, 0, 44, "acc4f0f346b2f5b76d2f2ad18718e40826dd3897f31f32e3353a5d6c877481d7"},
		{1, 3, 8, "74b226f4e60f2face0cf6e779442f857fc4a375d8d5fe1f757e4d2a75a05d303"},
		{1, 3, 17, "663fe1a200644fc3eef0fa67e820c24b9636c889f62f48ef69dba595e67db4af"},
		{1, 3, 44, "d33041eb8be8c2a5f9629284afbedf86c5ea5a0955fc9c0f2efd13e501bab2ea"},
		{2, 0, 8, "c370367637e025c42144e259f55a1791121b40916822cd2fe9a45fb543afa257"},
		{2, 0, 17, "1a0b1bc3cbb7c4bbe67f7d9592cb2328927c6f1bfeaae02ffbeaf0bcd513f6a3"},
		{2, 0, 44, "30e54fca8291aff14c3e5e5514378959235a6a0df8e4a3746dad73545b1870af"},
		{2, 3, 8, "dd89a72df88e966056bda085d348df41dc4869114e0607a6ed94387938619662"},
		{2, 3, 17, "046f01655e286e5893484c7dc0dd2405fe61035a4b7697c9a08512702cc5999c"},
		{2, 3, 44, "452febb96767df28d90314f72e20e2881f65cd5562dc016aa652244dcc6a2860"},
		{3, 0, 8, "563a690ea3657e474e37da83c2a93943eefc1fa76353b25e7d15e74414039a9a"},
		{3, 0, 17, "b47b53557b2d1a285abb7837eddbe08568466bc381f83a38b45f75a3c3429d2b"},
		{3, 0, 44, "9b896973c482baf4e6f6598e5e7627730a7321e5467d769ca4a93ee7262fe68f"},
		{3, 3, 8, "daa5bba79badc613b4b1f904b7b3005111e6c716482f56ec60e415bab8fe7949"},
		{3, 3, 17, "bd5c6d6b0c5b1c8782930e1eab09a076637a3bc60b9663d925d59941a2e8cb72"},
		{3, 3, 44, "ec41cffe62c568c78da9b45730ffae142ffe6da7ed44098a4fd58f62b0c91108"},
	}
	check := func(which, part, width int, digest string) {
		text, cuts := foldFixture(which)
		before := append([]convoCut{}, cuts[:cap(cuts)]...)
		c := &convoScreen{}
		c.cut(part, text, cuts, width)
		if !reflect.DeepEqual(cuts[:cap(cuts)], before) {
			t.Error("cut changed caller backing storage")
		}
		text, cuts = foldFixture(1)
		before = append([]convoCut{}, cuts[:cap(cuts)]...)
		c.cut(part+1, text, cuts, width)
		if !reflect.DeepEqual(cuts[:cap(cuts)], before) {
			t.Error("second cut changed caller backing storage")
		}
		data := foldScreenBytes(c)
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != digest {
			t.Errorf("fixture %d part %d width %d: %s want %s; %s", which, part, width, got, digest, data)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, tc := range cases {
		check(tc.which, tc.part, tc.width, tc.digest)
		wg.Add(1)
		go func(which, part, width int, digest string) {
			defer wg.Done()
			<-start
			for n := 0; n < 8; n++ {
				check(which, part, width, digest)
			}
		}(tc.which, tc.part, tc.width, tc.digest)
	}
	close(start)
	wg.Wait()
}

func TestConvoFoldKeepsFirstIdentityAndCompleteCopy(t *testing.T) {
	text, cuts := foldFixture(0)
	c := &convoScreen{}
	c.cut(0, text, cuts, 44)
	if len(c.msgs) != 3 {
		t.Fatal(c.msgs)
	}
	if m := c.msgs[1]; m.id != "0\x00turn-a\x00answer\x00cd4de79bc6c93295\x001" || m.raw != "body\n\ntail\n\ninvisible" || m.from != 4 || m.to != 10 {
		t.Fatalf("first folded message %+v", m)
	}
	if m := c.msgs[2]; m.id != "0\x00turn-a\x00answer\x00cd4de79bc6c93295\x002" || m.raw != "body\n\n" || m.from != 11 || m.to != 15 {
		t.Fatalf("duplicate first-body message %+v", m)
	}
	a := foldConvoCuts(text, cuts)
	b := foldConvoCuts(text, cuts)
	a[0].first.key = "changed result"
	a[0].segments[0] = "changed segment"
	if b[0].first.key != "turn-a" || b[0].segments[0] != "\nYOU\nhello" || cuts[0].key != "turn-a" {
		t.Fatal("folded results share mutable storage")
	}
	if got := foldConvoCuts("", nil); got != nil {
		t.Fatalf("empty fold is not nil: %+v", got)
	}
}

func TestConvoFoldManyFragmentsCopyBytes(t *testing.T) {
	for _, tc := range []struct {
		n      int
		digest string
	}{
		{1000, "369b2ff51dbf8a4c3851a718b44147c3c780217e01eab9ede824b04bb9005d7a"},
		{2000, "351724fd51bab61d6b0575ec2bbb7afff2e124dec417ae3bfd2d788b2e4f1650"},
	} {
		text, cuts := fragmentFixture(tc.n)
		folded := foldConvoCuts(text, cuts)
		want := "root" + strings.Repeat("\n\n"+strings.Repeat("x", 32), tc.n)
		if len(folded) != 1 || folded[0].raw != want {
			t.Fatal("joined fragment bytes differ")
		}
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(folded[0].raw))); got != tc.digest {
			t.Fatalf("joined hash %s want %s", got, tc.digest)
		}
	}
}
