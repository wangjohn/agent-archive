package statshtml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedElements are every element the page uses. A new one is a decision:
// nothing that loads (img, link, script, iframe, object, embed, audio, video,
// source, form, input, a) is in the list.
var allowedElements = []string{
	"html", "head", "meta", "title", "style", "body", "main", "header", "footer", "section", "div", "span",
	"h1", "h2", "p", "ul", "li", "dl", "dt", "dd", "table", "thead", "tbody", "tr", "th", "td", "details", "summary",
	"svg", "g", "defs", "pattern", "rect", "line", "circle", "text",
}

// checkPage parses the page as strict XML (the page is written to be valid
// XHTML as well as HTML, so a stray or unclosed tag is an error), and checks
// what a self-contained page must be: only allowed elements, no script, no
// event handler, no attribute that names another resource, unique ids that
// every reference finds, and a stylesheet that fetches nothing.
func checkPage(t *testing.T, page []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(page))
	dec.Strict = true
	var stack []string
	ids := map[string]bool{}
	var refs []string
	sawStyle := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the page is not well-formed: %v\n(near byte %d)", err, dec.InputOffset())
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			name := tok.Name.Local
			if !slices.Contains(allowedElements, name) {
				t.Errorf("element <%s> is not one the page may use", name)
			}
			// SVG's <title> is inside an svg element; HTML's is in head.
			stack = append(stack, name)
			for _, a := range tok.Attr {
				checkAttribute(t, name, a, ids, &refs)
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != tok.Name.Local {
				t.Fatalf("unbalanced </%s>", tok.Name.Local)
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1] == "style" {
				sawStyle = true
				checkStyle(t, string(tok))
			}
		}
	}
	if len(stack) != 0 {
		t.Fatalf("unclosed elements: %v", stack)
	}
	if !sawStyle {
		t.Error("the page has no inline stylesheet")
	}
	for _, ref := range refs {
		if !ids[ref] {
			t.Errorf("reference to id %q, which the page does not define", ref)
		}
	}
	text := string(page)
	for _, forbidden := range []string{"http://", "https://", "//cdn", "data:text", "@import", "<script", "<link", "<img", "<iframe", "<a "} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the page contains %q", forbidden)
		}
	}
	if !strings.HasPrefix(text, "<!DOCTYPE html>") {
		t.Error("the page does not start with the HTML5 doctype")
	}
}

func checkAttribute(t *testing.T, element string, a xml.Attr, ids map[string]bool, refs *[]string) {
	t.Helper()
	name := a.Name.Local
	switch {
	case strings.HasPrefix(name, "on"):
		t.Errorf("<%s> has the event handler %s", element, name)
	case name == "href", name == "src", name == "srcset", name == "action", name == "formaction", name == "data", name == "poster":
		t.Errorf("<%s> has %s=%q: the page loads and links nothing", element, name, a.Value)
	case name == "style":
		if strings.Contains(a.Value, "url(") || strings.Contains(a.Value, "expression") {
			t.Errorf("<%s> style %q fetches something", element, a.Value)
		}
	case name == "id":
		if ids[a.Value] {
			t.Errorf("duplicate id %q", a.Value)
		}
		ids[a.Value] = true
	case name == "aria-labelledby":
		*refs = append(*refs, strings.Fields(a.Value)...)
	}
}

var urlInStyle = regexp.MustCompile(`url\(([^)]*)\)`)

func checkStyle(t *testing.T, css string) {
	t.Helper()
	for _, m := range urlInStyle.FindAllStringSubmatch(css, -1) {
		if !strings.HasPrefix(m[1], "#") {
			t.Errorf("the stylesheet fetches %q; only url(#pattern) references are allowed", m[1])
		}
	}
	for _, bad := range []string{"@import", "@font-face", "expression(", "behavior:", "-moz-binding"} {
		if strings.Contains(css, bad) {
			t.Errorf("the stylesheet uses %s", bad)
		}
	}
}

// contrast is the WCAG contrast ratio of two #rrggbb colors.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := luminance(t, a), luminance(t, b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not a #rrggbb color: %q", hex)
	}
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		s := float64(v) / 255
		if s <= 0.03928 {
			c[i] = s / 12.92
		} else {
			c[i] = math.Pow((s+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

var cssVar = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;`)

// themeColors are the color tokens of the stylesheet's light theme (its first
// :root block) and of its dark theme (the light ones with the prefers-color-
// scheme: dark block laid over them).
func themeColors(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	css := styleSheet
	darkAt := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if darkAt < 0 {
		t.Fatal("the stylesheet has no dark theme")
	}
	light, dark = map[string]string{}, map[string]string{}
	for _, m := range cssVar.FindAllStringSubmatch(css[:darkAt], -1) {
		light[m[1]] = m[2]
		dark[m[1]] = m[2]
	}
	end := strings.Index(css[darkAt:], "* { box-sizing")
	if end < 0 {
		t.Fatal("cannot find the end of the dark theme block")
	}
	for _, m := range cssVar.FindAllStringSubmatch(css[darkAt:darkAt+end], -1) {
		dark[m[1]] = m[2]
	}
	return light, dark
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
