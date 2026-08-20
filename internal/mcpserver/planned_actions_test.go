package mcpserver_test

import (
	"encoding/json"
	"testing"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

func TestServer_PlannedActions_ContentMirrorsStructuredContent(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","plannedAction":{"type":"vaccination","description":"Прививка от бешенства","relatedProcedureName":"Прививка от бешенства","dueAmountMax":6,"dueUnit":"month"}}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "нужно сделать прививку от бешенства в течение полугода"})
	require.False(t, logResult.IsError, "%v", logResult.Content)

	result := callTool(t, session, "medical.planned_actions", map[string]any{"userId": "alex"})
	require.False(t, result.IsError, "%v", result.Content)
	out := requireContentMirrorsStructured[struct {
		Actions []struct {
			PlannedActionID string `json:"plannedActionId"`
			Type            string `json:"type"`
			Description     string `json:"description"`
			DueDateTo       string `json:"dueDateTo"`
			Overdue         bool   `json:"overdue"`
			Status          string `json:"status"`
			SourceType      string `json:"sourceType"`
			SourceID        string `json:"sourceId"`
		} `json:"actions"`
	}](t, result)

	require.Len(t, out.Actions, 1, "Content must carry the actual action, not just a count")
	require.Equal(t, "vaccination", out.Actions[0].Type)
	require.Equal(t, "pending", out.Actions[0].Status)
	require.False(t, out.Actions[0].Overdue)
	require.NotEmpty(t, out.Actions[0].DueDateTo)
	require.Equal(t, "self_reported", out.Actions[0].SourceType)
}

func TestServer_PlannedActions_DefaultExcludesDeclined(t *testing.T) {
	// One Structured call for log_event, one for the decline match — see
	// TestServer_DeclinePlannedAction_HappyPath's comment for why the
	// decline response is only known (and re-scripted) after log_event
	// returns the generated plannedActionId.
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","plannedAction":{"type":"vaccination","description":"Прививка от бешенства","relatedProcedureName":"Прививка от бешенства","dueAmountMax":6,"dueUnit":"month"}}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "нужно сделать прививку от бешенства"})
	require.False(t, logResult.IsError, "%v", logResult.Content)
	logOut := requireContentMirrorsStructured[struct {
		PlannedAction struct {
			PlannedActionID string `json:"plannedActionId"`
		} `json:"plannedAction"`
	}](t, logResult)
	require.NotEmpty(t, logOut.PlannedAction.PlannedActionID)

	provider.WithStructured(
		llmtest.StructuredResponse{}, // index 0, already consumed by log_event, never re-read
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"plannedActionId":"` + logOut.PlannedAction.PlannedActionID + `"}`)},
	)
	declineResult := callTool(t, session, "medical.decline_planned_action", map[string]any{"userId": "alex", "text": "отмени прививку от бешенства"})
	require.False(t, declineResult.IsError, "%v", declineResult.Content)

	defaultResult := callTool(t, session, "medical.planned_actions", map[string]any{"userId": "alex"})
	require.False(t, defaultResult.IsError)
	defaultOut := requireContentMirrorsStructured[struct {
		Actions []struct {
			Status string `json:"status"`
		} `json:"actions"`
	}](t, defaultResult)
	require.Empty(t, defaultOut.Actions, "declined actions must not appear by default")

	includeResolvedResult := callTool(t, session, "medical.planned_actions", map[string]any{"userId": "alex", "includeResolved": true})
	require.False(t, includeResolvedResult.IsError)
	includeResolvedOut := requireContentMirrorsStructured[struct {
		Actions []struct {
			Status string `json:"status"`
		} `json:"actions"`
	}](t, includeResolvedResult)
	require.Len(t, includeResolvedOut.Actions, 1)
	require.Equal(t, "declined", includeResolvedOut.Actions[0].Status)
}
