package surface

import (
	"net/http"
)

// ConnectorFormSchemaVersion is the D8 descriptor version (forward-compat
// switch for generic form renderers).
const ConnectorFormSchemaVersion = 1

// ConnectorFormValidation is the machine-readable validation grammar of one
// field. Every member is optional.
type ConnectorFormValidation struct {
	Pattern   string   `json:"pattern,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Min       *int     `json:"min,omitempty"`
	Max       *int     `json:"max,omitempty"`
	Reserved  []string `json:"reserved,omitempty"`
	// Rules are human-readable constraints beyond the structured members.
	Rules []string `json:"rules,omitempty"`
}

// ConnectorFormField describes one config key.
type ConnectorFormField struct {
	Key      string `json:"key"`
	Type     string `json:"type"` // string | string_list | secret_string | enum | int | duration
	Required bool   `json:"required"`
	// RequiredWhen makes the field required when another key has a value.
	RequiredWhen map[string]string        `json:"required_when,omitempty"`
	Default      any                      `json:"default"`
	Validation   *ConnectorFormValidation `json:"validation,omitempty"`
	Allowed      []string                 `json:"allowed,omitempty"`
	// Map marks a secret_string field that is an object of NAME -> value
	// (env); values are write-only and never echoed back.
	Map         bool   `json:"map,omitempty"`
	Description string `json:"description,omitempty"`
}

// ConnectorForm is the GET /v1/mcp/connector-form body.
type ConnectorForm struct {
	SchemaVersion int                  `json:"schema_version"`
	Fields        []ConnectorFormField `json:"fields"`
}

func intp(v int) *int { return &v }

// connectorFormDescriptor builds the static D8 descriptor from the same
// grammar constants validateConnectorConfig uses. It carries no per-host
// data and no secrets.
func connectorFormDescriptor() ConnectorForm {
	transports := append([]string(nil), ConnectorTransports...)
	reserved := append([]string(nil), connectorReservedNames...)
	return ConnectorForm{
		SchemaVersion: ConnectorFormSchemaVersion,
		Fields: []ConnectorFormField{
			{
				Key: "name", Type: "string", Required: true,
				Validation: &ConnectorFormValidation{
					Pattern: ConnectorNamePattern, MaxLength: ConnectorNameMaxLen, Reserved: reserved,
					Rules: []string{"must not collide with an existing connector (409 duplicate_name: offer Edit instead)"},
				},
				Description: "Connector name",
			},
			{
				Key: "transport", Type: "enum", Required: true,
				Allowed: transports, Description: "How the daemon reaches the tool server",
			},
			{
				Key: "command", Type: "string", Required: false,
				RequiredWhen: map[string]string{"transport": "stdio"},
				Validation: &ConnectorFormValidation{
					Rules: []string{"absolute path to an executable (stdio transport only)"},
				},
				Description: "Executable the daemon spawns",
			},
			{
				Key: "args", Type: "string_list", Default: []string{},
				Description: "Arguments passed to the command; reference secrets as ${vault:NAME}, never inline",
			},
			{
				Key: "url", Type: "string", Required: false,
				RequiredWhen: map[string]string{"transport": "http"},
				Validation: &ConnectorFormValidation{
					Rules: []string{
						"scheme must be http or https",
						"http is accepted only for loopback or RFC1918 hosts",
						"https is accepted for any host",
					},
				},
				Description: "Endpoint of the tool server (http transport only)",
			},
			{
				Key: "env", Type: "secret_string", Map: true,
				Description: "Environment variables NAME -> value (stdio); values are write-only and stored as vault references",
			},
			{
				Key: "bearer", Type: "secret_string",
				Description: "Bearer token for the http transport; write-only, stored as a vault reference",
			},
			{
				Key: "tool_allowlist", Type: "string_list", Default: []string{},
				Description: "When non-empty, only these tools are exposed",
			},
			{
				Key: "tool_denylist", Type: "string_list", Default: []string{},
				Description: "Tools that are never exposed",
			},
			{
				Key: "max_concurrent_calls", Type: "int", Default: ConnectorDefaultMaxConcurrent,
				Validation: &ConnectorFormValidation{
					Min: intp(ConnectorMinMaxConcurrent), Max: intp(ConnectorMaxMaxConcurrent),
				},
				Description: "Concurrent tool calls ceiling",
			},
			{
				Key: "list_timeout", Type: "duration", Default: ConnectorDefaultListTimeout,
				Validation: &ConnectorFormValidation{
					Rules: []string{"Go duration string, e.g. 30s"},
				},
				Description: "Budget for child boot and the first tools/list",
			},
		},
	}
}

// connectorFormHandler serves GET /v1/mcp/connector-form (D8). Static: it
// needs no door and answers even when custosd is unreachable.
func (s *Server) connectorFormHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSONAny(w, http.StatusOK, connectorFormDescriptor())
}
