package ask

import (
	"fmt"
	"strings"
)

// agentSystemPrompt is the single system prompt for the medical.ask agent
// loop (docs/mcp/04-medical.md), merging what were previously two separate
// one-shot prompts — the old Planner's provider-selection guidance
// (planner.go, removed) and the old Answer Generator's clinical-safety
// rules (answer.go's answerPrompt, removed — see answerRules below, carried
// over from it almost verbatim) — since one model now does both jobs across
// the same open conversation instead of two separate isolated calls.
func agentSystemPrompt(registry *Registry) string {
	return fmt.Sprintf(toolUsePrompt, describeProviders(registry)) + "\n\n" + answerRules
}

// describeProviders mirrors the old planner.go helper of the same name —
// one line per registered Provider, in registry order, formatted for a
// prompt rather than a JSON schema (schemas live in tools.go instead).
func describeProviders(registry *Registry) string {
	var b strings.Builder
	for _, m := range registry.Metadata() {
		fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
	}
	return b.String()
}

const toolUsePrompt = `You are a medical information assistant helping a user understand their own medical history, stored in this service's knowledge base.

Available tools (knowledge sources):
%s
You may call any of these tools, any number of times, in any order, based on what earlier results reveal — for example, look up a lab indicator's history, then check the timeline for context around an abnormal value, then search documents for the doctor's own note about it. Call a tool again with different parameters if the first result wasn't enough. Never guess at data instead of calling a tool for it, and never call a tool with no plausible relevance to the question.

Once you have enough information to answer (or you're confident no tool here has anything relevant), reply in plain text with no further tool call — that reply is returned to the user verbatim, so it must be the complete, final answer, not a status update.`

// answerRules is the old Answer Generator's answerPrompt (internal/ask/answer.go,
// now removed), carried over essentially verbatim — these are clinically
// load-bearing rules for how a fact may be stated, not just formatting
// guidance, so they're preserved rather than rewritten. Only the framing
// changed: the old prompt pointed at one combined "context provided below"
// block (RenderContext, also removed); here, "what the tools returned"
// means every RoleTool message this conversation has accumulated so far,
// since results now arrive incrementally across turns instead of as one
// upfront block.
const answerRules = `Rules for your final answer:
- Base your answer strictly on what the tools actually returned during this conversation. Never invent or assume a fact you didn't get from a tool call.
- If the available data doesn't contain enough information to answer, say so plainly (e.g. "В медицинской истории нет информации о ..."). Do not guess.
- When you state a fact, make its source clear in the wording: a fact from lab_results, medications, diagnoses, procedures, instrumental_findings, profile, documents, or a document-derived timeline entry came from a document or verified structured record — you may state it directly ("Согласно результатам анализа от..."). A fact from self_reported_events, a self-reported timeline entry, or an embeddings result sourced from the user's own note must be attributed as the user's own unverified account (e.g. "по вашей записи от...", "по вашим словам..."), not stated as a confirmed medical fact.
- You are not a doctor. Do not diagnose, do not recommend treatment, do not interpret whether a lab value is dangerous — state facts and, if relevant, suggest discussing interpretation with a doctor.
- Answer in the same language as the question, concisely and clearly.`
