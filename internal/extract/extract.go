// Package extract turns a lookup result (arbitrary JSON-shaped data) into a
// label value via a compiled jq query and an optional regex post-step.
package extract

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/itchyny/gojq"
)

// Query is a compiled jq expression plus an optional regex capture, ready
// to run repeatedly against different inputs.
type Query struct {
	src   string
	code  *gojq.Code
	regex *regexp.Regexp
}

// VarNames are bound as $labels / $annotations in every compiled query, so
// alert values reach jq without ever being interpolated into the expression
// text. Exported so config validation compiles a rule's jq under exactly
// the same variable bindings the engine will, rather than approximating it.
var VarNames = []string{"$labels", "$annotations"}

// Compile parses and compiles a jq expression. regexSrc may be empty.
func Compile(jqSrc, regexSrc string) (*Query, error) {
	parsed, err := gojq.Parse(jqSrc)
	if err != nil {
		return nil, fmt.Errorf("parse jq %q: %w", jqSrc, err)
	}
	code, err := gojq.Compile(parsed, gojq.WithVariables(VarNames))
	if err != nil {
		return nil, fmt.Errorf("compile jq %q: %w", jqSrc, err)
	}

	q := &Query{src: jqSrc, code: code}
	if regexSrc != "" {
		re, err := regexp.Compile(regexSrc)
		if err != nil {
			return nil, fmt.Errorf("compile regex %q: %w", regexSrc, err)
		}
		q.regex = re
	}
	return q, nil
}

// Run evaluates the query against input, with labels/annotations bound as
// $labels/$annotations. It returns ok=false when the query yields no value,
// null, or an empty string after the regex step — the caller decides
// whether that means "apply the default" or "fail".
func (q *Query) Run(input any, labels, annotations map[string]string) (value string, ok bool, err error) {
	iter := q.code.Run(input, toAny(labels), toAny(annotations))

	v, hasResult := iter.Next()
	if !hasResult {
		return "", false, nil
	}
	if err, isErr := v.(error); isErr {
		return "", false, fmt.Errorf("run jq %q: %w", q.src, err)
	}
	if v == nil {
		return "", false, nil
	}

	s, err := stringify(v)
	if err != nil {
		return "", false, fmt.Errorf("run jq %q: %w", q.src, err)
	}
	if s == "" {
		return "", false, nil
	}

	if q.regex != nil {
		m := q.regex.FindStringSubmatch(s)
		if m == nil {
			return "", false, nil
		}
		if len(m) > 1 {
			s = m[1]
		} else {
			s = m[0]
		}
		if s == "" {
			return "", false, nil
		}
	}

	return s, true, nil
}

func stringify(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case float64, int, bool:
		return fmt.Sprintf("%v", t), nil
	default:
		return "", fmt.Errorf("result has non-scalar type %T, expected a string/number/bool", v)
	}
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Cache compiles and memoizes queries by (jq, regex) pair so rules sharing
// the same expression (or reload cycles that re-parse an unchanged config)
// don't recompile it.
type Cache struct {
	mu    sync.Mutex
	byKey map[string]*Query
}

// NewCache returns an empty compiled-query cache.
func NewCache() *Cache {
	return &Cache{byKey: make(map[string]*Query)}
}

// Compile returns the cached Query for (jqSrc, regexSrc), compiling and
// storing it on first use.
func (c *Cache) Compile(jqSrc, regexSrc string) (*Query, error) {
	key := jqSrc + "\x00" + regexSrc
	c.mu.Lock()
	defer c.mu.Unlock()
	if q, ok := c.byKey[key]; ok {
		return q, nil
	}
	q, err := Compile(jqSrc, regexSrc)
	if err != nil {
		return nil, err
	}
	c.byKey[key] = q
	return q, nil
}
