package template_test

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/moostackhq/go/template"
)

const baseLayout = `<html><body>{{ template "content" . }}</body></html>`

func TestLoad_DefaultSection_BareKeys(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>home</p>{{ end }}`)},
		"tpl/_default/about.html":   &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>about</p>{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := set.Names()
	if len(names) != 2 || names[0] != "about" || names[1] != "home" {
		t.Errorf("Names() = %v, want [about home]", names)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "home", nil)
	if !strings.Contains(rec.Body.String(), "<p>home</p>") {
		t.Errorf("home render missing content: %q", rec.Body.String())
	}
}

func TestLoad_SectionShadowsDefaultLayout(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>DEFAULT {{ template "content" . }}</html>`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_layout.html":   &fstest.MapFile{Data: []byte(`<html>PUBLIC {{ template "content" . }}</html>`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "home", nil)
	if !strings.Contains(rec.Body.String(), "DEFAULT") {
		t.Errorf("home should use _default layout; got %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	set.Render(rec, "public/status", nil)
	if !strings.Contains(rec.Body.String(), "PUBLIC") {
		t.Errorf("public/status should use public layout; got %q", rec.Body.String())
	}
}

func TestLoad_SectionFallsBackToDefaultLayout(t *testing.T) {
	// errors/ has no _layout.html, so its page must inherit
	// _default/_layout.html.
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>FALLBACK {{ template "content" . }}</html>`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/errors/404.html":       &fstest.MapFile{Data: []byte(`{{ define "content" }}not found{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "errors/404", nil)
	got := rec.Body.String()
	if !strings.Contains(got, "FALLBACK") || !strings.Contains(got, "not found") {
		t.Errorf("errors/404 should pick up FALLBACK layout; got %q", got)
	}
}

func TestLoad_PartialsAreSharedFromDefault(t *testing.T) {
	// _default/_flash.html is a partial; pages in any section
	// should be able to {{ template "_flash" . }}.
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/_default/_flash.html":  &fstest.MapFile{Data: []byte(`{{ define "_flash" }}FLASH{{ end }}`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_layout.html":   &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "public/status"} {
		rec := httptest.NewRecorder()
		set.Render(rec, name, nil)
		if !strings.Contains(rec.Body.String(), "FLASH") {
			t.Errorf("%s missing _flash partial output: %q", name, rec.Body.String())
		}
	}
}

func TestLoad_SectionPartialShadowsDefault(t *testing.T) {
	// public/_flash.html shadows _default/_flash.html for pages
	// in public/.
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`<html>{{ template "_flash" . }}|{{ template "content" . }}</html>`)},
		"tpl/_default/_flash.html":  &fstest.MapFile{Data: []byte(`{{ define "_flash" }}DEFAULT-FLASH{{ end }}`)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
		"tpl/public/_flash.html":    &fstest.MapFile{Data: []byte(`{{ define "_flash" }}PUBLIC-FLASH{{ end }}`)},
		"tpl/public/status.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}status{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "home", nil)
	if !strings.Contains(rec.Body.String(), "DEFAULT-FLASH") {
		t.Errorf("home should see DEFAULT-FLASH; got %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	set.Render(rec, "public/status", nil)
	if !strings.Contains(rec.Body.String(), "PUBLIC-FLASH") {
		t.Errorf("public/status should see PUBLIC-FLASH; got %q", rec.Body.String())
	}
}

func TestLoad_DefaultFuncsAvailable(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 2 3 }}{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "page", nil)
	if got := rec.Body.String(); !strings.Contains(got, "5") {
		t.Errorf("add 2 3 = %q, want contains 5", got)
	}
}

func TestLoad_UserFuncsShadowDefaults(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 2 3 }}{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", template.FuncMap{
		"add": func(a, b int) int { return a * b },
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "page", nil)
	if got := rec.Body.String(); !strings.Contains(got, "6") {
		t.Errorf("user-supplied add should win; got %q", got)
	}
}

func TestLoad_VariadicFuncMapsMergeInOrder(t *testing.T) {
	// Later map wins. Last one supplied (custom) should beat both
	// the assetmapper-style first map and the library defaults.
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ greet }}|{{ asset "x" }}{{ end }}`)},
	}
	assetmapperLike := template.FuncMap{
		"asset": func(s string) string { return "/A/" + s },
		"greet": func() string { return "lib-greet" }, // overridden below
	}
	custom := template.FuncMap{
		"greet": func() string { return "custom-greet" },
	}
	set, err := template.Load(fsys, "tpl", assetmapperLike, custom)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "page", nil)
	got := rec.Body.String()
	if !strings.Contains(got, "custom-greet") {
		t.Errorf("custom map should override earlier map on collision; got %q", got)
	}
	if !strings.Contains(got, "/A/x") {
		t.Errorf("earlier map's non-colliding entry should still apply; got %q", got)
	}
}

func TestLoad_NoFuncMaps_DefaultsStillAvailable(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"tpl/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}{{ add 1 2 }}{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	set.Render(rec, "page", nil)
	if got := rec.Body.String(); !strings.Contains(got, "3") {
		t.Errorf("DefaultFuncs.add should still be available; got %q", got)
	}
}

func TestLoad_SectionWithoutLayoutAndNoFallback_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/only/home.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
	}
	if _, err := template.Load(fsys, "tpl", nil); err == nil {
		t.Fatal("expected error for section with pages but no layout (and no _default)")
	}
}

func TestLoad_MissingDir_Errors(t *testing.T) {
	fsys := fstest.MapFS{}
	if _, err := template.Load(fsys, "nope", nil); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestRender_UnknownPage_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		"tpl/_default/home.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}home{{ end }}`)},
	}
	set, err := template.Load(fsys, "tpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "missing", nil); err == nil {
		t.Error("Render of unknown page = nil error, want error")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("unknown page wrote %d bytes, want 0", rec.Body.Len())
	}
}

// TestRender_ExecError_NoPartialWrite is the regression for the
// silently-dropped render error: a template that fails at execution
// time must return an error with nothing written to w, so the caller
// can still send a clean 500 instead of a truncated 200.
func TestRender_ExecError_NoPartialWrite(t *testing.T) {
	fsys := fstest.MapFS{
		"tpl/_default/_layout.html": &fstest.MapFile{Data: []byte(baseLayout)},
		// boom returns an error at execution time, after the layout
		// has already emitted its opening bytes into the buffer.
		"tpl/_default/page.html": &fstest.MapFile{Data: []byte(`{{ define "content" }}<p>{{ boom }}</p>{{ end }}`)},
	}
	funcs := template.FuncMap{"boom": func() (string, error) { return "", errors.New("kaboom") }}
	set, err := template.Load(fsys, "tpl", funcs)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := set.Render(rec, "page", nil); err == nil {
		t.Error("Render with failing template = nil error, want error")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("failed render wrote %d bytes, want 0 (buffer must not flush on error)", rec.Body.Len())
	}
}
