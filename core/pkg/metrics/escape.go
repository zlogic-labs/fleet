package metrics

import "strings"

// Escaping for the text exposition format. Split out of text.go because the
// escaping rules are a separate thing from the writing of a family, and a
// change to one should not be read as a change to the other.
func escapeHelp(s string) string { return escape(s, false) }

func escapeLabel(s string) string { return escape(s, true) }

// escape implements the two escaping rules the format defines. A raw newline
// inside a label value splits the line in two and turns one series into two
// malformed ones, which is the failure a tenant id containing a newline would
// cause.
func escape(s string, quotes bool) string {
	if !strings.ContainsAny(s, "\\\n") && !(quotes && strings.Contains(s, `"`)) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '"':
			if quotes {
				b.WriteString(`\"`)
			} else {
				b.WriteByte('"')
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
