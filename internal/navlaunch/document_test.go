package navlaunch

import (
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestDocumentNestedLiteral(t *testing.T) {
	code := strings.Repeat("A", 43)
	body, err := Document(12345, "/invented/a b+&'é", code)
	if err != nil {
		t.Fatal(err)
	}
	fragment := "code=" + code + "&next=%2Fsessions%3Fdir%3D%252Finvented%252Fa%2Bb%252B%2526%2527%25C3%25A9"
	want := "http://127.0.0.1:12345/#" + fragment
	if strings.Count(string(body), html.EscapeString(want)) != 2 {
		t.Fatalf("nested encoding differs: %s", body)
	}
	fields, err := url.ParseQuery(fragment)
	if err != nil || fields.Get("next") != "/sessions?dir=%2Finvented%2Fa+b%2B%26%27%C3%A9" {
		t.Fatal("outer layer")
	}
	if strings.Contains(string(body), "<script") || !strings.Contains(string(body), `content="no-referrer"`) || !strings.Contains(string(body), "default-src 'none'") {
		t.Fatal("handoff effects")
	}
}

func TestDocumentRefusesWholeInput(t *testing.T) {
	for _, dir := range []string{"", "relative", "/a/../b", "/a/", "//a", "/a\\b", "/a\n", "/a\x7f", "/\xff", "/" + strings.Repeat("x", 4096), "/" + strings.Repeat("é", 1800)} {
		if b, e := Document(12345, dir, strings.Repeat("A", 43)); e == nil || len(b) != 0 {
			t.Fatal("invalid or oversized directory admitted")
		}
	}
	for _, code := range []string{"", strings.Repeat("A", 42), strings.Repeat("A", 42) + "B"} {
		if _, e := Document(12345, "/invented", code); e == nil {
			t.Fatal("invalid code admitted")
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, e := Document(port, "/invented", strings.Repeat("A", 43)); e == nil {
			t.Fatal("invalid port")
		}
	}
}
