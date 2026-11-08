package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ErrConfirmSensitive is returned when a change touches a sensitive setting
// without the caller having confirmed it.
var ErrConfirmSensitive = errors.New("sensitive setting needs confirmation")

// ErrEnvOverride is returned when a change would have no effect, because an
// environment variable overrides the key.
var ErrEnvOverride = errors.New("overridden by an environment variable")

// versionRe is a release tag; it goes into the URL the engine is downloaded from.
var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// normalizeQueue holds the queue to its absolute limits whatever the file or
// environment says, and keeps the queue large enough for its parallel tasks.
func normalizeQueue(q *QueueSettings) {
	parallel := min(max(q.Worker.MaxConcurrent, 0), MaxParallelTasks)
	if parallel != q.Worker.MaxConcurrent {
		log.Printf("config: %s=%d is outside 0-%d; using %d", keyMaxConcurrent, q.Worker.MaxConcurrent, MaxParallelTasks, parallel)
	}
	q.Worker.MaxConcurrent = parallel
	active := q.MaxActive
	if active <= 0 || active > MaxActiveTasks {
		log.Printf("config: %s=%d is outside 1-%d; using %d", keyMaxActive, active, MaxActiveTasks, MaxActiveTasks)
		active = MaxActiveTasks
	}
	q.MaxActive = max(active, max(parallel, 1))
}

// Runs reports whether the worker runs tasks: enabled, with at least one slot.
func (w QueueWorkerSettings) Runs() bool { return w.Enabled && w.MaxConcurrent > 0 }

// envName is the variable that overrides a key.
func envName(key string) string {
	return "ORCH_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// EnvOverridden reports whether an environment variable overrides key.
func EnvOverridden(key string) bool {
	_, ok := os.LookupEnv(envName(key))
	return ok
}

// ParseJSON decodes a client's JSON value for the field and validates it.
func ParseJSON(f Field, raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON: %w", f.Key, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", f.Key)
	}
	return checkValue(f, val)
}

// ParseText reads a command line's text for the field and validates it. Lists
// are comma-separated, maps are JSON.
func ParseText(f Field, text string) (any, error) {
	switch f.Type {
	case TypeBool, TypeInt, TypeFloat:
		return ParseJSON(f, text)
	case TypeList:
		var items []any
		for _, it := range strings.Split(text, ",") {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, it)
			}
		}
		return checkValue(f, items)
	case TypeMap:
		return ParseJSON(f, text)
	default:
		return checkValue(f, text)
	}
}

func checkValue(f Field, val any) (any, error) {
	bad := func(format string, args ...any) (any, error) {
		return nil, fmt.Errorf("%s: %s", f.Key, fmt.Sprintf(format, args...))
	}
	switch f.Type {
	case TypeBool:
		b, ok := val.(bool)
		if !ok {
			return bad("must be true or false")
		}
		return b, nil
	case TypeInt:
		n, ok := val.(json.Number)
		if !ok {
			return bad("must be a whole number")
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return bad("must be a whole number")
		}
		if f.HasRange && (float64(i) < f.Min || float64(i) > f.Max) {
			return bad("must be from %d to %d", int64(f.Min), int64(f.Max))
		}
		return int(i), nil
	case TypeFloat:
		n, ok := val.(json.Number)
		if !ok {
			return bad("must be a number")
		}
		x, err := n.Float64()
		if err != nil {
			return bad("must be a number")
		}
		if f.HasRange && (x < f.Min || x > f.Max) {
			return bad("must be from %g to %g", f.Min, f.Max)
		}
		return x, nil
	case TypeString, TypeDuration, TypeEnum, TypePath, TypeURL, TypeVersion:
		s, ok := val.(string)
		if !ok {
			return bad("must be text")
		}
		return checkText(f, s)
	case TypeList:
		return checkList(f, val)
	case TypeMap:
		return checkMap(f, val)
	}
	return bad("unknown type %q", f.Type)
}

func checkText(f Field, s string) (any, error) {
	bad := func(format string, args ...any) (any, error) {
		return nil, fmt.Errorf("%s: %s", f.Key, fmt.Sprintf(format, args...))
	}
	if strings.ContainsAny(s, "\x00\n\r") {
		return bad("must be one line")
	}
	switch f.Type {
	case TypeDuration:
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return bad("must be a positive duration such as 10m or 6h")
		}
	case TypeEnum:
		if !slices.Contains(f.Options, s) {
			return bad("must be one of %s", strings.Join(f.Options, ", "))
		}
	case TypeVersion:
		if !versionRe.MatchString(s) {
			return bad("must be a release such as v1.17.1")
		}
	case TypePath:
		if s != "" && !filepath.IsAbs(s) {
			return bad("must be an absolute path, or empty")
		}
	case TypeURL:
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad("must be an http or https URL")
		}
		// The whole config is written to a world-readable file.
		if u.User != nil {
			return bad("must not contain a username or password")
		}
	}
	return s, nil
}

func checkList(f Field, val any) (any, error) {
	if val == nil {
		return []string{}, nil
	}
	items, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a list of text", f.Key)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok || strings.TrimSpace(s) == "" || strings.ContainsAny(s, "\x00\n\r") {
			return nil, fmt.Errorf("%s: each item must be one non-empty line of text", f.Key)
		}
		out = append(out, s)
	}
	return out, nil
}

func checkMap(f Field, val any) (any, error) {
	m, ok := val.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a JSON object", f.Key)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		// Viper treats a dot as a path separator, so a key with one would land
		// somewhere else entirely.
		if k == "" || strings.ContainsAny(k, ".\x00\n\r") {
			return nil, fmt.Errorf("%s: invalid key %q", f.Key, k)
		}
		switch f.MapValues {
		case TypeFloat:
			n, ok := v.(json.Number)
			if !ok {
				return nil, fmt.Errorf("%s.%s: must be a number", f.Key, k)
			}
			x, err := n.Float64()
			if err != nil || x <= 0 || x > 10 {
				return nil, fmt.Errorf("%s.%s: must be above 0 and up to 10", f.Key, k)
			}
			out[k] = x
		case TypeList:
			list, err := checkList(Field{Key: f.Key + "." + k}, v)
			if err != nil {
				return nil, err
			}
			out[k] = list
		default:
			s, ok := v.(string)
			if !ok || strings.ContainsAny(s, "\x00\n\r") {
				return nil, fmt.Errorf("%s.%s: must be one line of text", f.Key, k)
			}
			out[k] = s
		}
	}
	return out, nil
}

// Apply validates parsed values against the queue's rules and persists them in
// one write: all of them or none. Raising parallel tasks turns the worker on
// and raises the queue limit with it, because a queue smaller than its parallel
// tasks would refuse the work those slots exist for.
func Apply(v *viper.Viper, values map[string]any, confirmSensitive bool) error {
	for key := range values {
		if EnvOverridden(key) {
			return fmt.Errorf("%w: %s is set by %s", ErrEnvOverride, key, envName(key))
		}
	}
	writes, err := planWrites(v, values, confirmSensitive)
	if err != nil {
		return err
	}
	return SetMany(v, writes)
}

// planWrites checks values against the current config v and returns every key
// to write, including the queue keys a change to the other drags along.
func planWrites(v *viper.Viper, values map[string]any, confirmSensitive bool) (map[string]any, error) {
	writes := make(map[string]any, len(values)+2)
	for key, val := range values {
		f, known := FieldFor(key)
		if known && f.Sensitive && !confirmSensitive {
			return nil, fmt.Errorf("%w: %s", ErrConfirmSensitive, key)
		}
		writes[key] = val
	}
	s, err := Unmarshal(v)
	if err != nil {
		return nil, err
	}
	parallel, active := s.Queue.Worker.MaxConcurrent, s.Queue.MaxActive
	newParallel, setParallel := values[keyMaxConcurrent].(int)
	if setParallel {
		parallel = newParallel
		if parallel > 0 {
			writes[keyWorkerEnabled] = true
		}
	}
	newActive, setActive := values[keyMaxActive].(int)
	if setActive {
		active = newActive
	}
	if parallel > active {
		if setActive {
			return nil, fmt.Errorf("%s (%d) cannot be lower than %s (%d)", keyMaxActive, active, keyMaxConcurrent, parallel)
		}
		writes[keyMaxActive] = parallel
	}
	return writes, nil
}

// Entry is a field with its current and default values, as JSON.
type Entry struct {
	Field
	Value, Default string
	EnvOverride    bool
}

// Entries returns every field clients are offered, in section order. defaults
// is a config with no file or environment applied, from Defaults.
func Entries(v, defaults *viper.Viper) ([]Entry, error) {
	var out []Entry
	for _, f := range Fields {
		if f.Hidden {
			continue
		}
		val, err := FieldValue(v, f)
		if err != nil {
			return nil, err
		}
		def, err := FieldValue(defaults, f)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Field: f, Value: val, Default: def, EnvOverride: EnvOverridden(f.Key)})
	}
	return out, nil
}

// Defaults returns a config holding only the compiled defaults.
func Defaults() *viper.Viper {
	v := viper.New()
	setDefaults(v)
	return v
}

// FieldValue is a field's effective value as JSON, in the shape ParseJSON
// accepts. The queue's fields show what the worker will actually do.
func FieldValue(v *viper.Viper, f Field) (string, error) {
	var val any
	switch f.Key {
	case keyMaxConcurrent, keyMaxActive:
		s, err := Unmarshal(v)
		if err != nil {
			return "", err
		}
		val = s.Queue.MaxActive
		if f.Key == keyMaxConcurrent {
			val = 0
			if s.Queue.Worker.Runs() {
				val = s.Queue.Worker.MaxConcurrent
			}
		}
	default:
		switch f.Type {
		case TypeBool:
			val = v.GetBool(f.Key)
		case TypeInt:
			val = v.GetInt(f.Key)
		case TypeFloat:
			val = v.GetFloat64(f.Key)
		case TypeList:
			val = listValue(v.Get(f.Key))
		case TypeMap:
			val = mapValue(v, f.Key)
		default:
			val = v.GetString(f.Key)
		}
	}
	b, err := json.Marshal(val)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.Key, err)
	}
	return string(b), nil
}

// listValue reads a list stored either as a list or as `orch config set`'s
// comma-separated text.
func listValue(raw any) []string {
	out := []string{}
	switch t := raw.(type) {
	case string:
		for _, it := range strings.Split(t, ",") {
			if it = strings.TrimSpace(it); it != "" {
				out = append(out, it)
			}
		}
	case []string:
		out = append(out, t...)
	case []any:
		for _, it := range t {
			out = append(out, fmt.Sprint(it))
		}
	}
	return out
}

// mapValue reads a map setting whatever Go type it arrived as. GetStringMap
// would turn a compiled default such as map[string]float64 into an empty map.
func mapValue(v *viper.Viper, key string) map[string]any {
	out := map[string]any{}
	b, err := json.Marshal(v.Get(key))
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		return map[string]any{}
	}
	if f, ok := FieldFor(key); ok && f.MapValues == TypeList {
		for k, raw := range out {
			out[k] = listValue(raw)
		}
	}
	return out
}

// Snapshot is every non-live field's effective value, to compare against the
// config a process started with.
func Snapshot(v *viper.Viper) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range Fields {
		if f.Live {
			continue
		}
		val, err := FieldValue(v, f)
		if err != nil {
			return nil, err
		}
		out[f.Key] = val
	}
	return out, nil
}

// PendingRestart lists, sorted, the keys whose value differs from a snapshot.
func PendingRestart(started, now map[string]string) []string {
	var out []string
	for k, val := range now {
		if started[k] != val {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
