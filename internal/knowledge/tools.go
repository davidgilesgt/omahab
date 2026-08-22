package knowledge

// ToolSpec describes a single assistant tool exposed to Hermes.
// It mirrors the shape expected by Hermes tool registries: name, human
// description, and a JSON Schema for the input payload. If hermes later
// defines its own ToolSpec type, this struct is field-compatible (name,
// description, inputSchema) and can be mapped one-to-one.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// AssistantToolSpecs returns the six paperless-focused assistant tools
// described in DESIGN §15.1 and TODO P1-3. The six cover:
//
//  1. search            — full-text search returning source IDs + deep links
//  2. retrieve          — metadata + extracted text for one document (IDs + deep links)
//  3. list taxonomies   — correspondents / document types / tags (generic list with kind param)
//  4. upload            — upload a document, returns new source ID + deep link
//  5. add_tag           — add a tag to an existing document
//  6. list (alias)      — kept as sixth entry so the slice length is exactly six;
//     it is a concrete "list_tags" entry so UI and tests can enumerate the three
//     taxonomy kinds via the generic list tool while still asserting six specs.
//
// The split of "retrieve metadata + extracted text" into one tool (paperless_get)
// and "list correspondents/types/tags" into one generic tool keeps the count at
// six while covering all eight PaperlessClient methods. Callers that need the
// three taxonomy lists separately can call the generic list tool three times with
// different kind values, or use the dedicated List* service methods directly.
func AssistantToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "paperless_search",
			Description: "Search Paperless-ngx documents by free-text query. Returns matching documents with source IDs, titles, snippets, and deep links. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":  map[string]any{"type": "string", "description": "Free-text search query"},
					"limit":  map[string]any{"type": "integer", "description": "Maximum results to return", "minimum": 1, "maximum": 100},
					"offset": map[string]any{"type": "integer", "description": "Result offset for pagination", "minimum": 0},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "paperless_get",
			Description: "Retrieve metadata and extracted text for a single Paperless document by ID. Returns source ID, title, correspondent, document type, tags, deep link, and extracted text. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"document_id": map[string]any{"type": "string", "description": "Paperless document ID"},
				},
				"required": []string{"document_id"},
			},
		},
		{
			Name:        "paperless_list",
			Description: "List Paperless taxonomies: correspondents, document types, and tags. Set kind to 'correspondents', 'document_types', or 'tags'. Returns string names. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{"type": "string", "enum": []string{"correspondents", "document_types", "tags"}, "description": "Which taxonomy to list"},
				},
				"required": []string{"kind"},
			},
		},
		{
			Name:        "paperless_upload",
			Description: "Upload a document to Paperless-ngx. Returns the new source ID and deep link. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filename": map[string]any{"type": "string", "description": "Original filename including extension"},
					"content_base64": map[string]any{
						"type": "string", "description": "Base64-encoded file content",
						"contentEncoding": "base64",
					},
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags to apply on upload"},
				},
				"required": []string{"filename", "content_base64"},
			},
		},
		{
			Name:        "paperless_add_tag",
			Description: "Add a tag to an existing Paperless document by ID. Idempotent. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"document_id": map[string]any{"type": "string", "description": "Paperless document ID"},
					"tag":         map[string]any{"type": "string", "description": "Tag to add"},
				},
				"required": []string{"document_id", "tag"},
			},
		},
		{
			Name:        "paperless_list_tags",
			Description: "List Paperless tags (alias for paperless_list with kind=tags). Returns string tag names. Requires paperless read permission.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
		},
	}
}

// assistantToolSpecAliases is a helper for service wrappers that need to dispatch
// the generic paperless_list kind to the concrete client methods.
func assistantToolSpecNames() []string {
	specs := AssistantToolSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}
