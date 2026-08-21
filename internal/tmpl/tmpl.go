// Package tmpl provides the text/template environment used to build
// lookup inputs (URLs, object names) and templated label values from an
// alert's labels and annotations.
package tmpl

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"text/template"
)

// Data is the value exposed to templates as ".".
type Data struct {
	Labels      map[string]string
	Annotations map[string]string
	StartsAt    string
}

var funcMap = template.FuncMap{
	"lower":    strings.ToLower,
	"upper":    strings.ToUpper,
	"trim":     strings.TrimSpace,
	"urlquery": url.QueryEscape,
	"default": func(def, val string) string {
		if val == "" {
			return def
		}
		return val
	},
}

// Compile parses a template using the enricher's function set.
func Compile(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(funcMap).Option("missingkey=zero").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse template %q: %w", name, err)
	}
	return t, nil
}

// Render compiles and executes a template in one step.
func Render(name, text string, data Data) (string, error) {
	t, err := Compile(name, text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %q: %w", name, err)
	}
	return buf.String(), nil
}

// Execute runs an already-compiled template. Rules compile their templates
// once at engine build time and execute them per alert, so the parse cost
// stays off the hot path — Render, which does both, is for one-shot uses.
func Execute(t *template.Template, data Data) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %q: %w", t.Name(), err)
	}
	return buf.String(), nil
}
