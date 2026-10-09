package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the **capability boundaries** of generic Webhook templates.
//
// This is the only place in the package where user-provided strings are evaluated as
// code. Its capabilities must be explicit and fixed by tests; otherwise a future
// method added to the template context or a readFile function added to FuncMap could
// silently expand its capabilities while looking like a harmless helper.

// TestTemplateContextHasNoMethods is the most important test.
//
// text/template calls exported methods: {{.Foo}} can access fields or call methods.
// Giving a template access to **any** type with exported methods exposes those
// methods to its author. This context deliberately contains only plain data (exported
// fields and no methods).
//
// A failure means a method was added to webhookTemplateData / webhookItem. Before
// allowing that, consider whether the method could expose data that should remain private.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can invoke them, "+
				"giving their capabilities to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to templates.
//
// Every additional FuncMap function adds a capability. Currently only json / jsons
// are exposed, to serialize values as JSON fragments; they cannot read files, send
// requests, or execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, expected %v. Before adding a function, "+
			"verify it does not expand capabilities (file access, network requests, or command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds template access: accessing
// unknown data must fail rather than expose anything, and the error must not reveal internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a nonexistent field should return an error")
	}
	// Errors must not contain real content from the template context (finding title/summary).
	for _, leak := range []string{"SQL injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently verifies that malformed templates are permanent
// config errors and will not recover with retries. If classified as retryable, one bad
// template would waste three backoff attempts on every delivery.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("template syntax errors should be caught when saving")
	}
	// Even if validation is bypassed and delivery is attempted, classify it as permanent rather than retrying.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("bad template should be a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON verifies that template output must be valid JSON.
// This also prevents using templates to generate plain text for other protocols.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// Valid templates should pass.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("valid template should pass validation: %v", err)
	}
	// Reject rendered non-JSON rather than sending it as-is.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendered non-JSON should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error should identify a JSON issue, got %v", err)
	}
}
