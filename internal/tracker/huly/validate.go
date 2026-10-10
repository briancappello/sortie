package huly

import (
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// parseEndpoint parses the configured Huly front URL. An empty value is
// unusable: Huly has no single hosted endpoint to default to.
func parseEndpoint(raw string) (httpkit.Endpoint, bool) {
	return httpkit.ParseEndpoint(raw)
}

// validateConfig checks Huly-specific configuration constraints for the
// sortie validate pipeline. It makes no network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	if _, ok := parseEndpoint(fields.Endpoint); !ok {
		diags = append(diags, registry.ValidationDiag{
			Severity: "error",
			Check:    "tracker.endpoint.invalid",
			Message:  `tracker.endpoint must be the absolute http(s) URL of the Huly front (e.g. "http://localhost:8087")`,
		})
	}

	if fields.Project != "" && typeutil.HasWhitespace(fields.Project) {
		diags = append(diags, registry.ValidationDiag{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be a Huly project identifier with no whitespace (e.g. "HULY")`,
		})
	}

	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityError)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityError)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}
