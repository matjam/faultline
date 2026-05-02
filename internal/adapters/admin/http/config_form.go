package adminhttp

import (
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/config"
)

// The configuration form is generated from the *config.Config struct
// via reflection rather than hand-maintained per field. Adding a new
// configuration field anywhere in internal/config wires it into the
// admin UI automatically: the reflection walk picks it up, classifies
// it by Go type, and emits the right input element.
//
// Type → input mapping:
//
//	bool                            → toggle ("[ ]" / "[X]")
//	intN, floatN                    → <input type="number">
//	string                          → <input type="text"> (or password
//	                                  for known-secret field names)
//	config.duration (alias int64)   → <input type="text"> with a
//	                                  parse-hint pattern
//	struct                          → recurse, with the toml-tag
//	                                  prefix prepended to each child
//	                                  field's path
//	[]SubagentProfile               → repeating sub-form, one row
//	                                  per existing element
//
// Secrets are detected by the field-name heuristic in isSecretField
// (suffix "Key" / "Password" / "Token"). Detection is conservative;
// adding new secret-like fields keeps working without code changes.
//
// Selects are driven by a small lookup keyed on the toml path. New
// enum-ish fields can opt in by adding a row to selectOptions.

// ConfigKind is the rendering tag the template switches on.
type ConfigKind string

const (
	KindBool     ConfigKind = "bool"
	KindInt      ConfigKind = "int"
	KindFloat    ConfigKind = "float"
	KindString   ConfigKind = "string"
	KindSecret   ConfigKind = "secret"
	KindDuration ConfigKind = "duration"
	KindSelect   ConfigKind = "select"
	// KindReadOnly is used for fields the form can't safely round-
	// trip (unsupported types). Rendered as a disabled input so the
	// operator can still see the current value but can't edit it.
	KindReadOnly ConfigKind = "readonly"
)

// ConfigField is a single editable (or read-only) row in the form.
type ConfigField struct {
	// Path is the dot-joined toml key, used as the form input
	// name. Examples: "api.url", "agent.max_tokens",
	// "subagent.profiles.0.name".
	Path string

	// Label is the short label shown next to the input. Defaults
	// to the toml-tag leaf when no friendlier name is supplied.
	Label string

	// Kind is the rendering tag.
	Kind ConfigKind

	// StringValue is the current value formatted for the input
	// element. For secrets the empty string is used; HasSecret
	// indicates whether a value already exists on disk.
	StringValue string

	// BoolValue is set when Kind == KindBool.
	BoolValue bool

	// HasSecret is true when Kind == KindSecret AND the on-disk
	// value is non-empty. Drives the "(set)" badge.
	HasSecret bool

	// Options is populated for KindSelect.
	Options []string

	// Help is operator-facing inline help text, optional.
	Help string
}

// ConfigSection groups fields by their top-level toml table
// ("[api]", "[agent]", ...). Sections are emitted in the order the
// fields appear in *config.Config so adding a new top-level table
// at a deliberate spot in the struct controls its placement.
type ConfigSection struct {
	// Path is the toml-tag prefix for this section, e.g. "api".
	Path string

	// Title is the human-facing title — the toml table header
	// followed by a short comment when one is known.
	Title string

	// Fields are the simple leaves directly under this section
	// (recursive structs are flattened in dotted notation).
	Fields []ConfigField

	// ProfileRows is the repeating-element editor for a slice of
	// structs. Empty for sections that don't have one.
	ProfileRows []ProfileRow
}

// ProfileRow is one repeating sub-record under a slice-of-structs
// section. Today only [subagent] uses this, but the renderer is
// generic over any []struct field.
type ProfileRow struct {
	Index  int
	Fields []ConfigField
}

// ConfigForm bundles everything the template needs.
type ConfigForm struct {
	Sections []ConfigSection
}

// secretSuffixes are the case-sensitive Go-field-name suffixes that
// flag a field as containing a secret. Reflective field names are
// stable (they're the Go identifier, not the toml tag), so this
// matches independent of the operator's tomlname casing.
var secretSuffixes = []string{"Key", "Password", "Token"}

func isSecretField(name string) bool {
	for _, suf := range secretSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// selectOptions maps a toml-path to its allowed values. Add new
// enum-ish fields here.
var selectOptions = map[string][]string{
	"log.level":           {"debug", "info", "warn", "error"},
	"update.restart_mode": {"exit", "self-exec", "command"},
}

// sectionTitles lets us label the top-level sections with a short
// trailing comment without poking at the struct tags. Optional;
// missing entries fall back to "[name]".
var sectionTitles = map[string]string{
	"api":        "[api] // primary LLM endpoint",
	"agent":      "[agent] // loop & sampler",
	"telegram":   "[telegram] // operator channel",
	"log":        "[log] // logging",
	"sandbox":    "[sandbox] // docker isolation",
	"email":      "[email] // imap",
	"limits":     "[limits] // content caps",
	"update":     "[update] // self-update",
	"mcp":        "[mcp] // model context protocol",
	"embeddings": "[embeddings] // semantic search",
	"skills":     "[skills] // agent skills",
	"subagent":   "[subagent] // delegation",
	"admin":      "[admin] // this UI",
}

// BuildConfigForm reflects over cfg and returns the form descriptor
// the template iterates. The reflection walk is read-only.
func BuildConfigForm(cfg *config.Config) ConfigForm {
	form := ConfigForm{}
	if cfg == nil {
		return form
	}

	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tomlTag := tomlName(sf)
		if tomlTag == "" || tomlTag == "-" {
			continue
		}

		section := ConfigSection{
			Path:  tomlTag,
			Title: sectionTitle(tomlTag),
		}

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			walkStruct(fv, sf.Type, tomlTag, &section.Fields, &section.ProfileRows)
		default:
			// Top-level scalar (we don't have any today, but
			// don't lose it if someone adds one).
			if f, ok := scalarField(fv, sf, tomlTag); ok {
				section.Fields = append(section.Fields, f)
			}
		}
		form.Sections = append(form.Sections, section)
	}
	return form
}

// walkStruct populates fields (and profile rows for slice-of-struct
// children) from v, using prefix as the toml-path stem.
func walkStruct(v reflect.Value, t reflect.Type, prefix string,
	fields *[]ConfigField, rows *[]ProfileRow,
) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := tomlName(sf)
		if name == "" || name == "-" {
			continue
		}
		path := prefix + "." + name

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			// Nested struct → flatten.
			walkStruct(fv, sf.Type, path, fields, rows)
		case reflect.Slice:
			if fv.Type().Elem().Kind() == reflect.Struct {
				rows2 := buildProfileRows(fv, path)
				*rows = append(*rows, rows2...)
				continue
			}
			// Other slice types: read-only display.
			f := ConfigField{
				Path:        path,
				Label:       leafLabel(name),
				Kind:        KindReadOnly,
				StringValue: fmt.Sprintf("%v", fv.Interface()),
				Help:        "list type — edit in TOML directly",
			}
			*fields = append(*fields, f)
		default:
			if f, ok := scalarField(fv, sf, path); ok {
				*fields = append(*fields, f)
			}
		}
	}
}

// buildProfileRows expands a []struct field into one ProfileRow per
// element. The element type is walked recursively just like a top-
// level struct, with paths of the shape "<prefix>.<idx>.<field>".
func buildProfileRows(v reflect.Value, prefix string) []ProfileRow {
	rows := make([]ProfileRow, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		row := ProfileRow{Index: i}
		elem := v.Index(i)
		walkStruct(elem, elem.Type(),
			fmt.Sprintf("%s.%d", prefix, i), &row.Fields, nil)
		rows = append(rows, row)
	}
	return rows
}

// scalarField builds a ConfigField for a leaf value of a primitive
// kind. Returns ok=false for kinds we don't know how to render.
func scalarField(fv reflect.Value, sf reflect.StructField, path string) (ConfigField, bool) {
	leaf := tomlLeaf(path)
	f := ConfigField{
		Path:  path,
		Label: leaf,
	}

	if isDuration(sf.Type) {
		f.Kind = KindDuration
		f.StringValue = time.Duration(fv.Int()).String()
		f.Help = "format: 5m, 1h30m, 90s"
		return f, true
	}

	switch fv.Kind() {
	case reflect.Bool:
		f.Kind = KindBool
		f.BoolValue = fv.Bool()
		return f, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.Kind = KindInt
		f.StringValue = strconv.FormatInt(fv.Int(), 10)
		return f, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.Kind = KindInt
		f.StringValue = strconv.FormatUint(fv.Uint(), 10)
		return f, true
	case reflect.Float32, reflect.Float64:
		f.Kind = KindFloat
		f.StringValue = strconv.FormatFloat(fv.Float(), 'g', -1, 64)
		return f, true
	case reflect.String:
		// Select?
		if opts, ok := selectOptions[path]; ok {
			f.Kind = KindSelect
			f.Options = opts
			f.StringValue = fv.String()
			return f, true
		}
		// Secret?
		if isSecretField(sf.Name) {
			f.Kind = KindSecret
			f.HasSecret = fv.String() != ""
			f.StringValue = ""
			return f, true
		}
		f.Kind = KindString
		f.StringValue = fv.String()
		return f, true
	default:
		return f, false
	}
}

// ApplyConfigForm walks the same struct shape and pulls form values
// back out. Mutates cfg in place. Secret fields are only overwritten
// when the form value is non-empty (operator left blank ⇒ keep the
// existing value).
//
// Returns a slice of warnings — fields whose values failed to parse
// keep their current value and a warning is recorded. The caller
// surfaces the warnings to the operator alongside the save result.
func ApplyConfigForm(cfg *config.Config, values url.Values) []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	v := reflect.ValueOf(cfg).Elem()
	applyStruct(v, v.Type(), "", values, &warnings)
	return warnings
}

func applyStruct(v reflect.Value, t reflect.Type, prefix string,
	values url.Values, warnings *[]string,
) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := tomlName(sf)
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			applyStruct(fv, sf.Type, path, values, warnings)
		case reflect.Slice:
			if fv.Type().Elem().Kind() == reflect.Struct {
				applySliceOfStruct(fv, path, values, warnings)
			}
			// Other slices: untouched.
		default:
			applyScalar(fv, sf, path, values, warnings)
		}
	}
}

func applySliceOfStruct(fv reflect.Value, prefix string, values url.Values,
	warnings *[]string,
) {
	// Mutate in place; we don't add/remove rows from the form yet.
	for i := 0; i < fv.Len(); i++ {
		elem := fv.Index(i)
		applyStruct(elem, elem.Type(),
			fmt.Sprintf("%s.%d", prefix, i), values, warnings)
	}
}

func applyScalar(fv reflect.Value, sf reflect.StructField, path string,
	values url.Values, warnings *[]string,
) {
	if isDuration(sf.Type) {
		raw := strings.TrimSpace(values.Get(path))
		if raw == "" {
			return
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("%s: %v", path, err))
			return
		}
		fv.SetInt(int64(d))
		return
	}

	switch fv.Kind() {
	case reflect.Bool:
		// Checkboxes only post when checked. We emit a hidden
		// "<path>__present" companion alongside every bool so we
		// can tell "form omitted this field" from "checkbox was
		// off". Without the present marker we leave the value
		// alone.
		if values.Get(path+"__present") == "" {
			return
		}
		fv.SetBool(values.Get(path) != "")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		raw, ok := optionalScalar(values, path)
		if !ok {
			return
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("%s: %v", path, err))
			return
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		raw, ok := optionalScalar(values, path)
		if !ok {
			return
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("%s: %v", path, err))
			return
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		raw, ok := optionalScalar(values, path)
		if !ok {
			return
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("%s: %v", path, err))
			return
		}
		fv.SetFloat(f)
	case reflect.String:
		if isSecretField(sf.Name) {
			// Empty = keep existing.
			raw := values.Get(path)
			if raw == "" {
				return
			}
			fv.SetString(raw)
			return
		}
		// Non-secret strings: only overwrite when the form
		// actually included the key. Some forms might omit a
		// field entirely (e.g. a partial submit) and we don't
		// want to clobber it with "".
		if !values.Has(path) {
			return
		}
		fv.SetString(values.Get(path))
	}
}

// optionalScalar returns (value, true) when the form included the
// field with a non-empty value. Empty strings or missing keys both
// translate to (_, false), leaving the existing value untouched.
func optionalScalar(values url.Values, path string) (string, bool) {
	if !values.Has(path) {
		return "", false
	}
	raw := strings.TrimSpace(values.Get(path))
	if raw == "" {
		return "", false
	}
	return raw, true
}

// tomlName returns the toml-tag identifier for sf, or sf.Name in
// snake-case fallback. Tags with options ("omitempty" etc.) are
// trimmed.
func tomlName(sf reflect.StructField) string {
	tag := sf.Tag.Get("toml")
	if tag == "" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// tomlLeaf returns the last segment of a dotted toml path. Used as
// a default label.
func tomlLeaf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// leafLabel returns a friendlier label for a leaf name. Today this
// is just the leaf as-is; kept as a hook for future polish.
func leafLabel(name string) string { return name }

// isDuration reports whether t is the unexported duration alias from
// internal/config. Detected by name + package path so we don't trip
// on time.Duration directly (which we don't actually use at the
// struct-field level today, but might in the future).
func isDuration(t reflect.Type) bool {
	return t.Name() == "duration" &&
		strings.HasSuffix(t.PkgPath(), "/internal/config")
}

func sectionTitle(name string) string {
	if t, ok := sectionTitles[name]; ok {
		return t
	}
	return "[" + name + "]"
}

// SectionPaths returns the section paths in order — used by tests to
// assert the form covers what we expect.
func (f ConfigForm) SectionPaths() []string {
	out := make([]string, len(f.Sections))
	for i, s := range f.Sections {
		out[i] = s.Path
	}
	return out
}

// AllFieldPaths returns every field path the form would emit, sorted.
// Convenient for tests that assert "this new config field shows up".
func (f ConfigForm) AllFieldPaths() []string {
	var out []string
	for _, s := range f.Sections {
		for _, fld := range s.Fields {
			out = append(out, fld.Path)
		}
		for _, r := range s.ProfileRows {
			for _, fld := range r.Fields {
				out = append(out, fld.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}
