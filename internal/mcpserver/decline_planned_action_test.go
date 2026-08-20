package mcpserver_test

import (
	"encoding/json"
	"testing"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

func TestServer_DeclinePlannedAction_HappyPath(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","plannedAction":{"type":"vaccination","description":"Прививка от бешенства","relatedProcedureName":"Прививка от бешенства","dueAmountMax":6,"dueUnit":"month"}}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "нужно сделать прививку от бешенства в течение полугода"})
	require.False(t, logResult.IsError, "%v", logResult.Content)
	logOut := requireContentMirrorsStructured[struct {
		PlannedAction struct {
			PlannedActionID string `json:"plannedActionId"`
		} `json:"plannedAction"`
	}](t, logResult)
	require.NotEmpty(t, logOut.PlannedAction.PlannedActionID)

	// The decline match response can only be scripted once the generated
	// plannedActionId is known — log_event and decline_planned_action share
	// one Pipeline/provider (both go through internal/pipeline.Pipeline's
	// single provider field), so this re-scripts FakeProvider's structured
	// script in place. FakeProvider.WithStructured reassigns the whole
	// script slice but does NOT reset its already-advanced call index, so
	// index 0 here (a throwaway zero value) is never re-read — only index
	// 1, the one the next Structured call actually consumes.
	provider.WithStructured(
		llmtest.StructuredResponse{},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"plannedActionId":"` + logOut.PlannedAction.PlannedActionID + `"}`)},
	)

	declineResult := callTool(t, session, "medical.decline_planned_action", map[string]any{"userId": "alex", "text": "отмени прививку от бешенства"})
	require.False(t, declineResult.IsError, "%v", declineResult.Content)
	declineOut := requireContentMirrorsStructured[struct {
		PlannedActionID string `json:"plannedActionId"`
		Status          string `json:"status"`
	}](t, declineResult)
	require.Equal(t, logOut.PlannedAction.PlannedActionID, declineOut.PlannedActionID)
	require.Equal(t, "declined", declineOut.Status)
}

func TestServer_DeclinePlannedAction_NoPendingActionsReturnsNotFound(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.decline_planned_action", map[string]any{"userId": "alex", "text": "отмени что угодно"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "PLANNED_ACTION_NOT_FOUND")
}

func TestServer_DeclinePlannedAction_EmptyTextRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.decline_planned_action", map[string]any{"userId": "alex", "text": "   "})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}
