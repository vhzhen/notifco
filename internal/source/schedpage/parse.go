// Package schedpage reads "Reserve:" notes from UW's public Schedule of Classes
// web page.
//
// This is a SECOND upstream, separate from the Open Data API, because the API
// does not carry these notes: a Class object has sixteen fields and none holds
// reserve text. The public page does, as an italic row directly beneath the
// section it applies to:
//
//	3234  LEC 081  ONLN ONLINE  ...
//	Reserve: Year 1 Math Students                   100   92
//
// The page is legacy HTML with no API, no robots.txt and no published terms.
// It requires no login, which is the line the project holds, but silence is not
// permission, so this package is deliberately conservative: on-demand only, one
// request per course, rate limited, cached, size capped, and fully optional.
// See client.go.
package schedpage

import (
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	// maxNoteRunes caps one reserve note. Real ones are short ("Year 1 Math
	// Students"); the cap bounds what a malformed or hostile page can inject.
	maxNoteRunes = 160
	// maxNotesPerSection bounds notes kept per section. PSYCH 101 sections have
	// four; this leaves room without letting a broken page balloon memory.
	maxNotesPerSection = 12
)

var (
	classNumberRE = regexp.MustCompile(`^\d{3,5}$`)
	// A section cell looks like "LEC 001" or "TST 201".
	sectionCellRE = regexp.MustCompile(`^[A-Z]{2,4} \d{3}$`)
	reserveRE     = regexp.MustCompile(`(?i)^reserve\s*:\s*(.*)$`)
)

// Page is what was extracted from one schedule page.
type Page struct {
	// Reserves maps a class number to its reserve notes, in page order, with the
	// "Reserve:" label removed.
	Reserves map[int][]string
	// Sections is how many section rows were recognised. Zero means the course
	// does not exist at that level, or the page layout changed.
	Sections int
	// Parsed is how many reserve rows were recognised (attached or not).
	Parsed int
	// Mentions is how many times the literal text "Reserve:" appears anywhere on
	// the page. If this is non-zero while Parsed is zero, the markup has almost
	// certainly changed and notes are being silently missed; see Drift.
	Mentions int
	// Unattached counts reserve rows that appeared before any section row.
	Unattached int
}

// Drift reports that the page mentions reserves but none were extracted, which
// is the signature of a layout change rather than a course with no reserves.
func (p *Page) Drift() bool { return p.Mentions > 0 && p.Parsed == 0 }

// Parse extracts reserve notes from a schedule page. It is tolerant by design:
// the markup is uppercase, unclosed and inconsistent, and the HTML5 parser
// copes with all of that. Malformed input yields fewer notes, never an error or
// a panic. The only error returned is a read failure.
func Parse(r io.Reader) (*Page, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	p := &Page{Reserves: make(map[int][]string)}

	current := 0 // class number of the section row most recently seen
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style":
				return // never read text out of these
			case "tr":
				current = p.row(n, current)
				// Do NOT return here. The real section rows live in a table
				// nested inside the outer course table's row, so the children
				// must still be walked. Returning early found zero sections on
				// every real page.
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	p.Mentions = strings.Count(strings.ToLower(pageText(doc)), "reserve:")
	return p, nil
}

// row classifies one table row and returns the class number to carry forward.
func (p *Page) row(tr *html.Node, current int) int {
	var cells []string
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
			cells = append(cells, cleanText(nodeText(c)))
		}
	}
	if len(cells) == 0 {
		return current
	}

	// A section row: class number, then "LEC 001".
	if len(cells) >= 2 && classNumberRE.MatchString(cells[0]) && sectionCellRE.MatchString(cells[1]) {
		n := atoi(cells[0])
		p.Sections++
		return n
	}

	// A reserve row: its first non-empty cell starts with "Reserve:".
	for _, c := range cells {
		if c == "" {
			continue
		}
		if m := reserveRE.FindStringSubmatch(c); m != nil {
			p.addReserve(current, m[1])
		}
		break
	}
	return current
}

func (p *Page) addReserve(class int, text string) {
	text = truncate(cleanText(text), maxNoteRunes)
	if text == "" {
		return
	}
	p.Parsed++
	if class == 0 {
		p.Unattached++
		return
	}
	notes := p.Reserves[class]
	for _, existing := range notes {
		if existing == text {
			return // an identical note repeated under one section
		}
	}
	if len(notes) >= maxNotesPerSection {
		return
	}
	p.Reserves[class] = append(notes, text)
}

// nodeText concatenates the text beneath n, skipping script and style.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func pageText(doc *html.Node) string { return nodeText(doc) }

// cleanText collapses whitespace (including the non-breaking spaces the page is
// full of), drops control characters and invalid UTF-8, and trims.
func cleanText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\u00a0':
			return ' '
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
