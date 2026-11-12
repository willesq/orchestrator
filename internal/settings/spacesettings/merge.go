package spacesettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Owned records what orch put in one settings file, so orch can take out
// exactly that later and never a user's own entries.
type Owned struct {
	// Lists are the entries orch added, by key path. An entry the file
	// already held is the user's and is not listed.
	Lists map[string][]string `json:"lists,omitempty"`
	// Scalars are the values orch set, by key path.
	Scalars map[string]Prior `json:"scalars,omitempty"`
	// Created are objects and arrays orch created, by key path; each is
	// removed again once empty.
	Created []string `json:"created,omitempty"`
	// File reports that orch created the file, which is removed again once
	// nothing is left in it.
	File bool `json:"file,omitempty"`
}

// Empty reports whether orch owns nothing in the file.
func (o Owned) Empty() bool {
	return len(o.Lists) == 0 && len(o.Scalars) == 0 && len(o.Created) == 0 && !o.File
}

// Prior is a value orch set and what was there before.
type Prior struct {
	// Set is the value orch wrote.
	Set json.RawMessage `json:"set"`
	// Was is the value before orch's; nil when the key was absent.
	Was json.RawMessage `json:"was,omitempty"`
}

// ErrInvalid is returned for a settings file orch cannot merge into: not
// JSON, or a key orch writes holding the wrong kind of value. The file is
// the user's to fix, so orch leaves it alone.
var ErrInvalid = errors.New("settings file cannot be merged")

// Merged is the result of Merge.
type Merged struct {
	// Data is the file's new content.
	Data []byte
	// Owned is what orch owns in it now.
	Owned Owned
	// Changed reports whether Data differs in meaning from the file.
	Changed bool
	// Remove reports that orch created the file and nothing is left in it.
	Remove bool
}

// Merge puts want into a settings file that held old (nil when absent),
// given what orch owned there before. Entries orch added that want no
// longer has are taken out, values orch set are put back as they were, and
// everything else in the file is left as it is. With an empty want it takes
// out all of orch's entries.
func Merge(old []byte, exists bool, want Fragment, own Owned) (Merged, error) {
	root, err := parseObject(old)
	if err != nil {
		return Merged{}, err
	}
	m := merger{root: root, own: cloneOwned(own)}
	if !exists {
		m.own.File = !want.Empty()
	}
	if err := m.dropUnwanted(want); err != nil {
		return Merged{}, err
	}
	if err := m.addWanted(want); err != nil {
		return Merged{}, err
	}
	m.pruneCreated()
	out := Merged{Owned: m.own, Changed: m.changed}
	if m.own.File && len(m.root) == 0 && want.Empty() {
		out.Remove, out.Changed = true, exists
		out.Owned.File = false
		return out, nil
	}
	if !m.changed {
		out.Data = old
		return out, nil
	}
	out.Changed = true
	out.Data, err = encode(m.root)
	return out, err
}

type merger struct {
	root    map[string]any
	own     Owned
	changed bool
}

// dropUnwanted takes out what orch added or set that want no longer has.
func (m *merger) dropUnwanted(want Fragment) error {
	for _, key := range sortedKeys(m.own.Lists) {
		keep := want.Lists[key]
		var still []string
		for _, v := range m.own.Lists[key] {
			if slices.Contains(keep, v) {
				still = append(still, v)
				continue
			}
			if err := m.removeEntry(key, v); err != nil {
				return err
			}
		}
		setOrDelete(m.own.Lists, key, still)
	}
	for _, key := range sortedKeys(m.own.Scalars) {
		if _, ok := want.Scalars[key]; ok {
			continue
		}
		p := m.own.Scalars[key]
		cur, ok, err := m.get(key)
		if err != nil {
			return err
		}
		// A value the user changed since is theirs now and stays.
		if ok && sameJSON(cur, p.Set) {
			if p.Was == nil {
				m.del(key)
			} else {
				var v any
				if err := decode(p.Was, &v); err != nil {
					return err
				}
				if err := m.set(key, v); err != nil {
					return err
				}
			}
			m.changed = true
		}
		delete(m.own.Scalars, key)
	}
	return nil
}

// addWanted adds want's entries and values. A list entry the file already
// holds and orch did not add stays the user's.
func (m *merger) addWanted(want Fragment) error {
	for _, key := range sortedKeys(want.Lists) {
		list, err := m.list(key)
		if err != nil {
			return err
		}
		owned := m.own.Lists[key]
		for _, v := range want.Lists[key] {
			if slices.Contains(list, any(v)) {
				continue
			}
			list = append(list, v)
			if !slices.Contains(owned, v) {
				owned = append(owned, v)
			}
			m.changed = true
		}
		if err := m.set(key, list); err != nil {
			return err
		}
		setOrDelete(m.own.Lists, key, owned)
	}
	for _, key := range sortedKeys(want.Scalars) {
		v := want.Scalars[key]
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		cur, ok, err := m.get(key)
		if err != nil {
			return err
		}
		if ok && sameJSON(cur, raw) {
			continue
		}
		p, owned := m.own.Scalars[key]
		if !owned {
			p = Prior{}
			if ok {
				if p.Was, err = json.Marshal(cur); err != nil {
					return err
				}
			}
		}
		p.Set = raw
		if m.own.Scalars == nil {
			m.own.Scalars = map[string]Prior{}
		}
		m.own.Scalars[key] = p
		if err := m.set(key, v); err != nil {
			return err
		}
		m.changed = true
	}
	return nil
}

// pruneCreated removes objects and arrays orch created that are now empty,
// deepest first.
func (m *merger) pruneCreated() {
	created := slices.Clone(m.own.Created)
	sort.Slice(created, func(i, j int) bool { return strings.Count(created[i], ".") > strings.Count(created[j], ".") })
	var keep []string
	for _, key := range created {
		v, ok, err := m.get(key)
		if err != nil || !ok {
			continue
		}
		if isEmpty(v) {
			m.del(key)
			continue
		}
		keep = append(keep, key)
	}
	slices.Sort(keep)
	m.own.Created = keep
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// get returns the value at key.
func (m *merger) get(key string) (any, bool, error) {
	parts := strings.Split(key, ".")
	obj := m.root
	for i, p := range parts[:len(parts)-1] {
		next, ok := obj[p]
		if !ok {
			return nil, false, nil
		}
		o, isObj := next.(map[string]any)
		if !isObj {
			return nil, false, fmt.Errorf("%w: %s is not an object", ErrInvalid, strings.Join(parts[:i+1], "."))
		}
		obj = o
	}
	v, ok := obj[parts[len(parts)-1]]
	return v, ok, nil
}

// set puts v at key, creating objects on the way and recording them.
func (m *merger) set(key string, v any) error {
	parts := strings.Split(key, ".")
	obj := m.root
	for i, p := range parts[:len(parts)-1] {
		next, ok := obj[p]
		if !ok {
			o := map[string]any{}
			obj[p] = o
			m.created(strings.Join(parts[:i+1], "."))
			obj = o
			continue
		}
		o, isObj := next.(map[string]any)
		if !isObj {
			return fmt.Errorf("%w: %s is not an object", ErrInvalid, strings.Join(parts[:i+1], "."))
		}
		obj = o
	}
	last := parts[len(parts)-1]
	if _, ok := obj[last]; !ok {
		if _, isList := v.([]any); isList {
			m.created(key)
		}
	}
	obj[last] = v
	return nil
}

// del removes key, if there.
func (m *merger) del(key string) {
	parts := strings.Split(key, ".")
	obj := m.root
	for _, p := range parts[:len(parts)-1] {
		o, ok := obj[p].(map[string]any)
		if !ok {
			return
		}
		obj = o
	}
	delete(obj, parts[len(parts)-1])
}

func (m *merger) created(key string) {
	if !slices.Contains(m.own.Created, key) {
		m.own.Created = append(m.own.Created, key)
	}
}

// list returns the array at key, empty when absent.
func (m *merger) list(key string) ([]any, error) {
	v, ok, err := m.get(key)
	if err != nil || !ok {
		return []any{}, err
	}
	l, isList := v.([]any)
	if !isList {
		return nil, fmt.Errorf("%w: %s is not a list", ErrInvalid, key)
	}
	return l, nil
}

// removeEntry takes every copy of v out of the list at key.
func (m *merger) removeEntry(key, v string) error {
	l, err := m.list(key)
	if err != nil {
		return err
	}
	n := slices.DeleteFunc(slices.Clone(l), func(e any) bool { return e == any(v) })
	if len(n) == len(l) {
		return nil
	}
	m.changed = true
	return m.set(key, n)
}

func setOrDelete(m map[string][]string, key string, v []string) {
	if len(v) == 0 {
		delete(m, key)
		return
	}
	m[key] = v
}

func cloneOwned(o Owned) Owned {
	out := Owned{Lists: map[string][]string{}, Scalars: map[string]Prior{}, Created: slices.Clone(o.Created), File: o.File}
	for k, v := range o.Lists {
		out.Lists[k] = slices.Clone(v)
	}
	for k, v := range o.Scalars {
		out.Scalars[k] = v
	}
	return out
}

// parseObject decodes a settings file, keeping numbers as written. An empty
// or missing file is an empty object.
func parseObject(b []byte) (map[string]any, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return root, nil
	}
	if err := decode(b, &root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if root == nil {
		return nil, fmt.Errorf("%w: not a JSON object", ErrInvalid)
	}
	return root, nil
}

func decode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// encode writes settings the way orch's other settings writers do: two-space
// indent, a trailing newline, and <, > and & left as they are.
func encode(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sameJSON reports whether a decoded value encodes as raw.
func sameJSON(v any, raw json.RawMessage) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var x, y any
	if decode(b, &x) != nil || decode(raw, &y) != nil {
		return false
	}
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(bx, by)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
