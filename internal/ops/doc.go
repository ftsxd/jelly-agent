// Package ops holds the domain model for the SRE diagnosis agent: the types
// every other layer is written against.
//
//	IncidentContext  what we are diagnosing, resolved to concrete handles
//	ToolMetadata     what a tool is, beyond its JSON schema
//	Evidence         one observation, with the provenance that makes it checkable
//	DiagnosisResult  the conclusion, with every claim pointing back at evidence
//
// Two rules give these types their value, and both are enforced in code rather
// than asked of the model.
//
// Evidence is minted by the tool gateway, never by the agent. The gateway is
// the single place that stamps provenance (which tool, which args, which time
// window, when), applies shaping and redaction, and de-duplicates identical
// calls. A deterministic workflow step and a model-chosen call therefore
// produce evidence with identical semantics, and the same recording can be
// replayed offline for evaluation.
//
// A claim is "validated" only if it cites evidence that exists.
// DiagnosisResult.Seal walks every claim, drops references to evidence that is
// not in the result, and demotes any validated claim left citing nothing. The
// model proposes citations; code decides which ones count.
//
// Nothing here imports an agent framework, an MCP client, or a transport.
// Swapping ADK for something else must not touch these files.
package ops
