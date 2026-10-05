package frontline

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The page's columns, by header text; their order doesn't matter.
const (
	colTime    = "Event Time"
	colSurface = "Surface"
	colHome    = "Home or Event"
	colAway    = "Away or Event"
)

var (
	// "Daily Events for Sunday Oct 04, 2026"
	headingRe = regexp.MustCompile(`^Daily Events for [A-Za-z]+ ([A-Z][a-z]{2} \d{2}, \d{4})$`)
	// "07:15 A - 08:45 A", "11:00 A - 12:00 P"
	timesRe = regexp.MustCompile(`^(\d{1,2}):(\d{2}) ?([AP])M? ?- ?(\d{1,2}):(\d{2}) ?([AP])M?$`)
)

func parse(page []byte, date time.Time, loc *time.Location) ([]Event, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("schedule page: %w", err)
	}

	// The heading names the day shown. If it's not the one asked for, the
	// site ignored the date (and would show today instead).
	h := find(doc, func(n *html.Node) bool { return n.DataAtom == atom.H4 && headingRe.MatchString(text(n)) })
	if h == nil {
		return nil, errors.New("schedule page has no date heading (an error page, or a new layout)")
	}
	if shown, want := headingRe.FindStringSubmatch(text(h))[1], date.Format("Jan 02, 2006"); shown != want {
		return nil, fmt.Errorf("schedule page shows %s, not %s", shown, want)
	}

	table := find(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Table && find(n, func(th *html.Node) bool {
			return th.DataAtom == atom.Th && text(th) == colTime
		}) != nil
	})
	if table == nil {
		return nil, errors.New("schedule table not found")
	}

	var cols map[string]int
	events := []Event{} // non-nil: an empty day is not a failure
	for tr := range table.Descendants() {
		if tr.DataAtom != atom.Tr {
			continue
		}
		if ths := children(tr, atom.Th); len(ths) > 0 {
			cols = map[string]int{}
			for i, th := range ths {
				cols[text(th)] = i
			}
			for _, c := range []string{colTime, colSurface, colHome, colAway} {
				if _, ok := cols[c]; !ok {
					return nil, fmt.Errorf("schedule table has no %q column", c)
				}
			}
			continue
		}
		tds := children(tr, atom.Td)
		if len(tds) == 0 {
			continue
		}
		if cols == nil {
			return nil, errors.New("schedule table has rows before its header")
		}
		if len(tds) != len(cols) {
			return nil, fmt.Errorf("schedule row has %d cells, want %d", len(tds), len(cols))
		}
		e := Event{
			Surface: text(tds[cols[colSurface]]),
			Home:    side(tds[cols[colHome]]),
			Away:    side(tds[cols[colAway]]),
		}
		raw := text(tds[cols[colTime]])
		if e.Start, e.End, err = times(raw, date, loc); err != nil {
			return nil, err
		}
		if e.Surface == "" {
			return nil, fmt.Errorf("schedule row at %s has no surface", raw)
		}
		events = append(events, e)
	}
	if cols == nil {
		return nil, errors.New("schedule table has no header")
	}
	return events, nil
}

// times parses "07:15 A - 08:45 A" on date. An end at or before the start
// is taken to be after midnight.
func times(s string, date time.Time, loc *time.Location) (start, end time.Time, err error) {
	m := timesRe.FindStringSubmatch(s)
	if m == nil {
		return start, end, fmt.Errorf("can't read event time %q", s)
	}
	y, mo, d := date.Date()
	clock := func(h, min, ampm string) (time.Time, error) {
		hh, _ := strconv.Atoi(h)
		mm, _ := strconv.Atoi(min)
		if hh < 1 || hh > 12 || mm > 59 {
			return time.Time{}, fmt.Errorf("can't read event time %q", s)
		}
		hh %= 12
		if ampm == "P" {
			hh += 12
		}
		return time.Date(y, mo, d, hh, mm, 0, 0, loc), nil
	}
	if start, err = clock(m[1], m[2], m[3]); err != nil {
		return
	}
	if end, err = clock(m[4], m[5], m[6]); err != nil {
		return
	}
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	return start, end, nil
}

// side reads a Home or Away cell: a name, then optionally a
// <div>Locker: 2</div>.
func side(td *html.Node) Side {
	var s Side
	var name []string
	for c := range td.ChildNodes() {
		t := text(c)
		if c.Type == html.ElementNode && strings.HasPrefix(t, "Locker:") {
			s.Locker = strings.TrimSpace(strings.TrimPrefix(t, "Locker:"))
			continue
		}
		name = append(name, t)
	}
	s.Name = clean(strings.Join(name, " "))
	return s
}

func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	for d := range n.Descendants() {
		if match(d) {
			return d
		}
	}
	return nil
}

func children(n *html.Node, a atom.Atom) []*html.Node {
	var out []*html.Node
	for c := range n.ChildNodes() {
		if c.DataAtom == a {
			out = append(out, c)
		}
	}
	return out
}

// text is n's text content, whitespace collapsed.
func text(n *html.Node) string {
	if n.Type == html.TextNode {
		return clean(n.Data)
	}
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
			b.WriteByte(' ')
		}
	}
	return clean(b.String())
}

func clean(s string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(s, "�")), " ")
}
