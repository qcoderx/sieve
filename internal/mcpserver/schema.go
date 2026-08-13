package mcpserver

import "encoding/json"

// The manifest schema is written by hand instead of being reflected.
//
// Two tools return a manifest, and the SDK reflects the whole emit.Manifest
// struct into each of them: sections, counts, stats, audit, provenance and
// every nested type, expanded in full, twice. That came to 3,983 characters per
// tool -- around two thousand tokens of every session's context spent
// describing the same struct to the model before it has read a single page, in
// an ecosystem where teams are uninstalling MCP servers over exactly this cost.
//
// What a model needs from an output schema is the shape and where to look: that
// there is an outcome and it decides whether the rest is worth reading, that
// sections carry ids and token costs, that counts say what the whole thing
// would cost. It does not need the field-by-field expansion of the audit
// sub-object, which it will read in the response if it ever wants it.
//
// This is deliberately permissive -- no required fields, no additionalProperties
// restriction -- because AddTool validates real responses against it and a
// tighter schema would reject perfectly good output. It describes; it does not
// constrain.
// shape declares a tool's output in one line instead of expanding it.
//
// An expanded schema for a struct as large as the manifest costs around two
// thousand characters, and the SDK emits one per tool that returns it. What the
// model actually needs -- read the outcome first, sections carry ids and token
// costs, fetch by id -- is in Instructions, which is sent once per session
// rather than once per tool, and in the manifest's own guidance field, which
// arrives with the first response. Saying it a third time in seven schemas is
// the same sentence bought seven times.
//
// Permissive by construction: AddTool validates real responses against this,
// and a schema that constrained anything would eventually reject good output.
func shape(desc string) json.RawMessage {
	b, err := json.Marshal(map[string]any{"type": "object", "description": desc})
	if err != nil {
		// Unreachable for a string literal. A broken schema is worse than none.
		return nil
	}
	return b
}

// manifestShape is the one-line form of what distill and status return.
const manifestShape = "job_id, state, and on completion a manifest: outcome (read first: " +
	"status ok|blocked|challenge|auth_required|spa_shell|empty_after_render|partial, with evidence " +
	"and http_status), title, summary, sections (id, title, est_tokens -- fetch by id with " +
	"get_content), counts including est_total_tokens, gaps, provenance, audit."

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
