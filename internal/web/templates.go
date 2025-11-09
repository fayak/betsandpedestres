package web

import (
	"embed"
	"html/template"
	"io"
	"path/filepath"
	"time"

	"github.com/Masterminds/sprig/v3"
)

//go:embed tpl/**/*.tmpl
//go:embed tpl/*.tmpl
var tplFS embed.FS

type Renderer struct{}

func NewRenderer() (*Renderer, error) { return &Renderer{}, nil }

func (r *Renderer) Render(w io.Writer, name string, data any) error {
	funcs := template.FuncMap{
		"nowUTC":      func() time.Time { return time.Now().UTC() },
		"formatCoins": func(v int64) string { return strconvFormat(v) },
	}
	t := template.New("root").Funcs(funcs).Funcs(sprig.FuncMap())
	if _, err := t.ParseFS(tplFS, "tpl/base.tmpl", "tpl/partials/*.tmpl"); err != nil {
		return err
	}
	pagePath := filepath.Join("tpl/pages", name+".tmpl")
	if _, err := t.ParseFS(tplFS, pagePath); err != nil {
		return err
	}
	return t.ExecuteTemplate(w, name, data)
}

func strconvFormat(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
