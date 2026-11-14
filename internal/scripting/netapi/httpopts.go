package netapi

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// The parsers below read the timeout, redirects and params options of a
// script's HTTP call. JavaScript and Python both hand them the plain
// value the script gave (a number, a string, a map or a list), so both
// languages accept and refuse the same things. A nil value means the
// option was not given. The caller prefixes the option's name.

// ParseTimeout reads a per-call time limit: a duration string such as
// "500ms" or "5s", or a number counted in unit (milliseconds in
// JavaScript, seconds in Python). It must be above zero.
func ParseTimeout(v any, unit time.Duration) (time.Duration, error) {
	var d time.Duration
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		parsed, err := time.ParseDuration(x)
		if err != nil {
			return 0, fmt.Errorf("want a duration such as \"500ms\" or \"5s\", not %q", x)
		}
		d = parsed
	default:
		n, ok := number(v)
		if !ok {
			return 0, fmt.Errorf("want a number or a duration such as \"5s\", not %v", v)
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n*float64(unit) > math.MaxInt64 {
			return 0, fmt.Errorf("%v is not a usable time limit", n)
		}
		d = time.Duration(n * float64(unit))
	}
	if d <= 0 {
		return 0, fmt.Errorf("want a time limit above zero, not %v", v)
	}
	return d, nil
}

// ParseRedirects reads how many redirects a call follows: a whole number,
// 0 or more. 0 means the call returns the 3xx answer itself.
func ParseRedirects(v any) (*int, error) {
	if v == nil {
		return nil, nil
	}
	n, ok := number(v)
	if !ok || n != math.Trunc(n) || n < 0 || n > math.MaxInt32 {
		return nil, fmt.Errorf("want a whole number, 0 or more (0 does not follow redirects), not %v", v)
	}
	r := int(n)
	return &r, nil
}

// ParseParams reads query parameters: a map from name to a string, a
// number, true or false, or a list of those (the name is repeated once
// per item).
func ParseParams(v any) (url.Values, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("want an object of name: value pairs, like {page: 2}")
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make(url.Values, len(m))
	for _, k := range names {
		items, isList := m[k].([]any)
		if !isList {
			items = []any{m[k]}
		}
		for _, item := range items {
			s, ok := paramText(item)
			if !ok {
				return nil, fmt.Errorf("%s: want a string, a number, true or false, or a list of them, not %v", k, item)
			}
			out.Add(k, s)
		}
	}
	return out, nil
}

func paramText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	}
	if n, ok := number(v); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return strconv.FormatFloat(n, 'f', -1, 64), true
	}
	return "", false
}

// number reads a value a script runtime hands over as a number. A bool is
// not one, though Python counts True as 1.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
