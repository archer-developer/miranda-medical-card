package mcpserver_test

import (
	"encoding/json"
	"testing"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

func TestServer_CompletePlannedAction_HappyPath(t *testing.T) {
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

	// See TestServer_DeclinePlannedAction_HappyPath for why the match
	// response can only be scripted once the generated plannedActionId is
	// known, and why the throwaway index-0 response is needed.
	provider.WithStructured(
		llmtest.StructuredResponse{},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"matchId":"` + logOut.PlannedAction.PlannedActionID + `"}`)},
	)

	completeResult := callTool(t, session, "medical.complete_planned_action", map[string]any{"userId": "alex", "text": "я сделал прививку от бешенства"})
	require.False(t, completeResult.IsError, "%v", completeResult.Content)
	completeOut := requireContentMirrorsStructured[struct {
		PlannedActionID string `json:"plannedActionId"`
		Status          string `json:"status"`
	}](t, completeResult)
	require.Equal(t, logOut.PlannedAction.PlannedActionID, completeOut.PlannedActionID)
	require.Equal(t, "completed", completeOut.Status)
}

func TestServer_CompletePlannedAction_NoPendingActionsReturnsNotFound(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.complete_planned_action", map[string]any{"userId": "alex", "text": "я сделал что угодно"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "PLANNED_ACTION_NOT_FOUND")
}

func TestServer_CompletePlannedAction_EmptyTextRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.complete_planned_action", map[string]any{"userId": "alex", "text": "   "})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}
