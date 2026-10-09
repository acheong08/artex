package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is a generic Webhook adapter with user-configurable URL, method,
// headers, and JSON template. It avoids dedicated implementations for Slack,
// Mattermost, Discord, and self-hosted systems; a configurable template can cover them.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// Generic Webhooks have no official rate limit. Return 0 for no default limit so users
// can set one based on the receiving service's capacity.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// Mask url and headers: destination URLs often contain tokens, and custom headers
// commonly contain credentials. Both are returned by the API, so both must be masked.
// The tradeoff is that changing one header requires resubmitting the whole set (masked
// values mean "keep the stored value"); this is deliberate to avoid exposing credentials
// to the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// url is the destination. When changing it, headers must be explicitly resubmitted;
// otherwise the original Authorization header would be sent to the new destination,
// the main way to bypass masking.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the fallback request body when no template is set: a simple
// JSON structure suitable for most self-hosted receivers that ingest JSON.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context exposed to user templates.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is the delivery time (RFC3339), for the receiver's records.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is a readable status-change description, e.g. "Pending -> Fixed";
	// empty for non-status-change events.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("destination URL is required")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("invalid destination URL: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("unsupported method %s (supported: GET/POST/PUT/PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("invalid request body template syntax: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET has no request body. Putting content in the query exceeds template capabilities
	// and violates GET semantics, so GET is suitable only for receivers where a match
	// triggers a hook.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// Templates render JSON as a string. Convert it to json.RawMessage and send it as-is,
		// avoiding double-escaping the user's structure into a JSON string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("rendered request body template is not valid JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// Allow overrides, but apply them after headers so explicit config takes precedence.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// Do not truncate generic Webhook bodies (the receiver is user-owned and body_template
	// determines the size), so count the entire batch as delivered.
	return len(m.Items), nil
}

// renderWebhookBody renders the request body using the user template (or the default).
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("invalid request body template syntax: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("failed to render request body template: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate parses a template.
//
// missingkey=zero renders missing map keys as zero values instead of errors. The
// context here is a struct, so this mainly ensures ranging over an empty .Items works;
// the actual case to guard against is .Items being nil.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs returns helper functions exposed to templates.
var webhookTemplateFuncs = template.FuncMap{
	// json serializes any value as JSON.
	//
	// This function is essential: without it, users can only interpolate {{.Title}}
	// directly. A quote or newline in a finding title would make the entire request
	// body invalid JSON; the receiver would reject it with a "JSON parse failed" error
	// that gives no hint that the title contained a quote.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons embeds a JSON fragment inside another JSON string value (one level of string escaping).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Remove the outer quotes; callers decide whether to add quotes.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
