package spacesettings

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

const userLocal = `{
  "permissions": {"allow": ["Bash(make:*)"], "deny": ["Read(//Users/me/secret/**)"]},
  "env": {"FOO": "bar", "TMPDIR": "/mine"},
  "sandbox": {"enabled": false, "filesystem": {"denyRead": ["/Users/me/secret"]}},
  "model": "opus",
  "cleanupPeriodDays": 30
}
`

func testFragment() Fragment {
	return ClaudeFragment(Rules{Space: "joba", Deny: []string{"/Users/me/secret", "/w/b"}, TmpDir: "/d/joba/tmp", GHConfigDir: "/gh/a"})
}

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return m
}

func at(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		o, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = o[k]
	}
	return v
}

// TestMergePreservesUserEntries (SP-046.1.a2): the user's own entries stay,
// including ones that equal orch's, which orch then does not claim.
func TestMergePreservesUserEntries(t *testing.T) {
	m, err := Merge([]byte(userLocal), true, testFragment(), Owned{})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeMap(t, m.Data)
	if at(got, "model") != "opus" || at(got, "env", "FOO") != "bar" || at(got, "cleanupPeriodDays") != 30.0 {
		t.Errorf("user keys lost: %s", m.Data)
	}
	if !slices.Contains(at(got, "permissions", "allow").([]any), any("Bash(make:*)")) {
		t.Errorf("user allow lost: %s", m.Data)
	}
	deny := at(got, "permissions", "deny").([]any)
	if !slices.Contains(deny, any("Read(//Users/me/secret/**)")) || !slices.Contains(deny, any("Read(//w/b/**)")) {
		t.Errorf("deny = %v", deny)
	}
	if n := countOf(deny, "Read(//Users/me/secret/**)"); n != 1 {
		t.Errorf("user's deny written %d times", n)
	}
	if slices.Contains(m.Owned.Lists[keyPermDeny], "Read(//Users/me/secret/**)") {
		t.Errorf("orch claims the user's own deny: %v", m.Owned.Lists)
	}
	if at(got, "sandbox", "enabled") != true || at(got, "env", "TMPDIR") != "/d/joba/tmp" {
		t.Errorf("orch values not set: %s", m.Data)
	}
	if string(m.Owned.Scalars[keySandboxOn].Was) != "false" || string(m.Owned.Scalars["env.TMPDIR"].Was) != `"/mine"` {
		t.Errorf("prior values not kept: %+v", m.Owned.Scalars)
	}

	// Taking orch's entries out restores the file's meaning exactly.
	back, err := Merge(m.Data, true, Fragment{}, m.Owned)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMeaning(t, back.Data, []byte(userLocal)) {
		t.Errorf("after removal:\n%s\nwant\n%s", back.Data, userLocal)
	}
	if !back.Owned.Empty() || back.Remove {
		t.Errorf("owned after removal %+v remove=%v", back.Owned, back.Remove)
	}
}

func countOf(l []any, s string) int {
	n := 0
	for _, v := range l {
		if v == s {
			n++
		}
	}
	return n
}

func sameMeaning(t *testing.T, a, b []byte) bool {
	t.Helper()
	x, _ := json.Marshal(decodeMap(t, a))
	y, _ := json.Marshal(decodeMap(t, b))
	return string(x) == string(y)
}

// TestMergeIdempotent (SP-046.1.a2): merging again changes nothing, byte
// for byte, and claims nothing new.
func TestMergeIdempotent(t *testing.T) {
	first, err := Merge([]byte(userLocal), true, testFragment(), Owned{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Merge(first.Data, true, testFragment(), first.Owned)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || string(second.Data) != string(first.Data) {
		t.Errorf("second merge changed the file:\n%s\n%s", first.Data, second.Data)
	}
	a, _ := json.Marshal(first.Owned)
	b, _ := json.Marshal(second.Owned)
	if string(a) != string(b) {
		t.Errorf("owned changed: %s -> %s", a, b)
	}
}

// TestMergeNewFile: orch creates the file, then removes it when its entries
// go and nothing of the user's was added; a file the user added to stays.
func TestMergeNewFile(t *testing.T) {
	m, err := Merge(nil, false, testFragment(), Owned{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Changed || !m.Owned.File {
		t.Fatalf("new file: %+v", m)
	}
	gone, err := Merge(m.Data, true, Fragment{}, m.Owned)
	if err != nil || !gone.Remove || !gone.Changed {
		t.Fatalf("removal: %+v %v", gone, err)
	}
	withUser := decodeMap(t, m.Data)
	withUser["model"] = "opus"
	b, _ := json.Marshal(withUser)
	kept, err := Merge(b, true, Fragment{}, m.Owned)
	if err != nil || kept.Remove {
		t.Fatalf("user's addition removed: %+v %v", kept, err)
	}
	if got := decodeMap(t, kept.Data); len(got) != 1 || got["model"] != "opus" {
		t.Errorf("left %s", kept.Data)
	}
	none, err := Merge(nil, false, Fragment{}, Owned{})
	if err != nil || none.Changed || none.Remove || none.Data != nil {
		t.Errorf("empty merge on no file: %+v %v", none, err)
	}
}

// TestMergeFollowsPolicyChanges: an entry orch no longer wants goes, a value
// the user changed after orch set it stays the user's.
func TestMergeFollowsPolicyChanges(t *testing.T) {
	m, err := Merge(nil, false, testFragment(), Owned{})
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeMap(t, m.Data)
	doc["env"].(map[string]any)["GH_CONFIG_DIR"] = "/user/choice"
	b, _ := json.Marshal(doc)
	next := ClaudeFragment(Rules{Space: "joba", Deny: []string{"/w/c"}, TmpDir: "/d/joba/tmp"})
	m2, err := Merge(b, true, next, m.Owned)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeMap(t, m2.Data)
	deny := at(got, "permissions", "deny").([]any)
	if slices.Contains(deny, any("Read(//w/b/**)")) || !slices.Contains(deny, any("Read(//w/c/**)")) {
		t.Errorf("deny = %v", deny)
	}
	if at(got, "env", "GH_CONFIG_DIR") != "/user/choice" {
		t.Errorf("user's changed value replaced: %s", m2.Data)
	}
	if _, ok := m2.Owned.Scalars["env.GH_CONFIG_DIR"]; ok {
		t.Errorf("orch still owns a value the user changed")
	}
	// The user turning the sandbox off is overridden while orch enforces,
	// and the user's value comes back when orch's entries go.
	doc = decodeMap(t, m2.Data)
	doc["sandbox"].(map[string]any)["enabled"] = false
	b, _ = json.Marshal(doc)
	m3, err := Merge(b, true, next, m2.Owned)
	if err != nil || at(decodeMap(t, m3.Data), "sandbox", "enabled") != true {
		t.Fatalf("sandbox not enforced: %s %v", m3.Data, err)
	}
}

// TestMergeRefusesBadShapes: a key orch writes holding the wrong kind of
// value, or a file that is not a JSON object, is left for the user.
func TestMergeRefusesBadShapes(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":          `{`,
		"array":             `[]`,
		"null":              `null`,
		"trailing":          `{} {}`,
		"sandbox string":    `{"sandbox": "on"}`,
		"deny not list":     `{"permissions": {"deny": "x"}}`,
		"filesystem number": `{"sandbox": {"filesystem": 3}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Merge([]byte(doc), true, testFragment(), Owned{}); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v", err)
			}
		})
	}
	own := Owned{Scalars: map[string]Prior{"sandbox.x.enabled": {Set: json.RawMessage("true")}}}
	if _, err := Merge([]byte(`{"sandbox":{"x":1}}`), true, Fragment{}, own); !errors.Is(err, ErrInvalid) {
		t.Errorf("removal through a non-object: %v", err)
	}
	own = Owned{Lists: map[string][]string{keyPermDeny: {"x"}}}
	if _, err := Merge([]byte(`{"permissions":{"deny":1}}`), true, Fragment{}, own); !errors.Is(err, ErrInvalid) {
		t.Errorf("list removal from a non-list: %v", err)
	}
}

// TestMergeRestoresPriorValue: a value orch set over the user's comes back
// when orch's entries go, and a bad stored prior value is an error.
func TestMergeRestoresPriorValue(t *testing.T) {
	own := Owned{Scalars: map[string]Prior{keySandboxOn: {Set: json.RawMessage("true"), Was: json.RawMessage("false")}}}
	m, err := Merge([]byte(`{"sandbox":{"enabled":true}}`), true, Fragment{}, own)
	if err != nil || at(decodeMap(t, m.Data), "sandbox", "enabled") != false {
		t.Fatalf("%s %v", m.Data, err)
	}
	own.Scalars[keySandboxOn] = Prior{Set: json.RawMessage("true"), Was: json.RawMessage("{")}
	if _, err := Merge([]byte(`{"sandbox":{"enabled":true}}`), true, Fragment{}, own); err == nil {
		t.Error("bad prior accepted")
	}
}
