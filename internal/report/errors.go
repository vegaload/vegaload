package report

import (
	"sort"

	"github.com/vegaload/vegaload/internal/failure"
)

// errorKinds counts failures by kind (see failure.Kind) and keeps the first
// sample message of each. A run has only a handful of kinds, so it stays
// small. Its owner guards it with a lock.
type errorKinds map[string]*ErrorKind

// add counts one failure of kind. The sample is worked out only for the
// first failure of a kind, so a run of a million failures formats one
// message per kind.
func (e *errorKinds) add(kind string, n int64, sample func() string) {
	if kind == "" || n == 0 {
		return
	}
	if *e == nil {
		*e = errorKinds{}
	}
	k := (*e)[kind]
	if k == nil {
		k = &ErrorKind{Kind: kind}
		(*e)[kind] = k
	}
	k.Count += n
	if k.Sample == "" && sample != nil {
		k.Sample = sample()
	}
}

// addErr counts err, which failed for reason (see failure.Reason).
func (e *errorKinds) addErr(reason string, err error) {
	e.add(failure.KindOf(reason), 1, func() string { return failure.Sample(err) })
}

// merge adds the kinds of another report, keeping the first sample of each.
func (e *errorKinds) merge(list []ErrorKind) {
	for _, k := range list {
		e.add(k.Kind, k.Count, func() string { return k.Sample })
	}
}

// list is the kinds, most first, then by name. It is nil when there are
// none, so omitempty drops the JSON key.
func (e errorKinds) list() []ErrorKind {
	if len(e) == 0 {
		return nil
	}
	out := make([]ErrorKind, 0, len(e))
	for _, k := range e {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// copy returns a copy that shares nothing with e.
func (e errorKinds) copy() errorKinds {
	if len(e) == 0 {
		return nil
	}
	out := make(errorKinds, len(e))
	for k, v := range e {
		c := *v
		out[k] = &c
	}
	return out
}
