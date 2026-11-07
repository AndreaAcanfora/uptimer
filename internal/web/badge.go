package web

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/AndreaAcanfora/uptimer/internal/monitor"
)

// badge serves a shields-style SVG: /badge/<id>.svg shows up/down,
// /badge/<id>.svg?kind=uptime shows the 30-day uptime. ?label= overrides the left text.
func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".svg")
	if !ok {
		http.NotFound(w, r)
		return
	}
	var state *monitor.State
	for _, st := range s.Monitor.States() {
		if st.Check.ID == id {
			state = &st
			break
		}
	}
	if state == nil {
		http.NotFound(w, r)
		return
	}
	label := r.URL.Query().Get("label")
	var value, color string
	if r.URL.Query().Get("kind") == "uptime" {
		if label == "" {
			label = "uptime 30d"
		}
		ratio, _, err := s.Store.Uptime(r.Context(), id, s.now().Add(-30*24*time.Hour))
		if err != nil {
			s.fail(w, err)
			return
		}
		value, color = Percent(ratio), uptimeColor(ratio)
	} else {
		if label == "" {
			label = state.Check.Name
		}
		value, color = string(state.Status), statusColor(state.Status)
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	// Short cache so README badges stay fresh behind GitHub's image proxy.
	w.Header().Set("Cache-Control", "max-age=60, s-maxage=60")
	_, _ = w.Write([]byte(renderBadge(label, value, color)))
}

func statusColor(status monitor.Status) string {
	switch status {
	case monitor.Up:
		return "#2da44e"
	case monitor.Down:
		return "#cf222e"
	default:
		return "#8c959f"
	}
}

func uptimeColor(ratio float64) string {
	switch {
	case ratio < 0:
		return "#8c959f"
	case ratio >= 0.999:
		return "#2da44e"
	case ratio >= 0.99:
		return "#9a6700"
	default:
		return "#cf222e"
	}
}

// renderBadge draws a two-part badge. Widths are estimated from the text length,
// which is close enough for the short labels badges use.
func renderBadge(label, value, color string) string {
	textWidth := func(s string) int { return len([]rune(s))*7 + 12 }
	left, right := textWidth(label), textWidth(value)
	total := left + right
	label, value = html.EscapeString(label), html.EscapeString(value)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]d" height="20" role="img" aria-label="%[4]s: %[5]s">
<title>%[4]s: %[5]s</title>
<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="%[1]d" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)"><rect width="%[2]d" height="20" fill="#555"/><rect x="%[2]d" width="%[3]d" height="20" fill="%[6]s"/><rect width="%[1]d" height="20" fill="url(#s)"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="%[7]d" y="14">%[4]s</text><text x="%[8]d" y="14">%[5]s</text></g></svg>`,
		total, left, right, label, value, color, left/2, left+right/2)
}
