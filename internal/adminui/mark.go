package adminui

import (
	"encoding/base64"
	"html/template"
	"net/http"
	"strings"
)

// markPetal is one of the mark's six petals. The mark is an asterisk: in the
// Encyclopédie a star at the head of an article marks one Diderot supplied
// as its editor. static/favicon.svg draws the same petals on the page's
// ground.
const markPetal = "M0-1.1C-1.3-3.6-2.5-7.2-1.9-9.4-1.5-10.9 1.5-10.9 1.9-9.4 2.5-7.2 1.3-3.6 0-1.1Z"

// markSVG is the mark inline, in the current text colour, for the header and
// the sign-in page.
var markSVG = func() template.HTML {
	var b strings.Builder
	b.WriteString(`<svg class="brand-mark" viewBox="-12 -12 24 24" aria-hidden="true" focusable="false"><g fill="currentColor">`)
	for _, angle := range []string{"0", "60", "120", "180", "240", "300"} {
		b.WriteString(`<path d="` + markPetal + `" transform="rotate(` + angle + `)"/>`)
	}
	b.WriteString(`</g></svg>`)
	return template.HTML(b.String())
}()

// faviconURL is static/favicon.svg as a data URL, so a page shows the icon
// before its viewer is signed in to fetch anything from /admin/static.
// Browsers that do not read an SVG icon (older Safari) and iOS's home
// screen take the icon files icons serves instead.
var faviconURL = func() template.URL {
	svg, err := assets.ReadFile("static/favicon.svg")
	if err != nil {
		panic("adminui: favicon: " + err.Error())
	}
	return template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg))
}()

// icons are the mark as icon files, served to anyone (a signed-out browser
// asks for them on the sign-in page): by path, the static file and its type,
// named here so it does not depend on the host's MIME table.
var icons = map[string]struct{ file, contentType string }{
	"/auth/favicon.ico":          {"static/favicon.ico", "image/x-icon"},
	"/auth/apple-touch-icon.png": {"static/apple-touch-icon.png", "image/png"},
}

// mountIcons serves icons.
func mountIcons(mux Mux) {
	for path, icon := range icons {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", icon.contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFileFS(w, r, assets, icon.file)
		})
	}
}
