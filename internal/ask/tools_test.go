package ask_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

// allProviderNames mirrors every ask.Provider registered in
// cmd/miranda-medical-card/main.go's ask.NewRegistry call — kept as an
// explicit list here (rather than iterating a live Registry) so this test
// fails loudly if a new provider is added without a corresponding schema
// decision in tools.go's toolParameters.
var allProviderNames = []string{
	"timeline", "self_reported_events",
	"medications", "diagnoses", "procedures", "profile",
	"lab_results", "instrumental_findings",
	"documents", "embeddings",
}

func registryOfAllProviders() *ask.Registry {
	providers := make([]ask.Provider, len(allProviderNames))
	for i, name := range allProviderNames {
		providers[i] = fakeProvider{name: name}
	}
	return ask.NewRegistry(providers...)
}

func TestToolDefsForRegistry_OneToolPerProvider(t *testing.T) {
	// toolDefsForRegistry is unexported — exercised indirectly through
	// Asker.Ask's first Chat call, whose ChatRequest.Tools this test
	// inspects via a scripted ChatProvider, since there's no other exported
	// seam into per-provider schema construction from an ask_test
	// (external) file.
	registry := registryOfAllProviders()
	fake := &recordingChatProvider{text: "no lookup needed"}
	sessions := ask.NewSessionStore(nil)
	asker := ask.NewAsker(fake, registry, sessions, testProviderTimeout, 20, 8, nil)

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "irrelevant question")
	require.NoError(t, err)
	require.Len(t, fake.lastReq.Tools, len(allProviderNames))

	byName := make(map[string]int, len(fake.lastReq.Tools))
	for _, tool := range fake.lastReq.Tools {
		byName[tool.Name]++
	}
	for _, name := range allProviderNames {
		require.Equal(t, 1, byName[name], "expected exactly one tool named %q", name)
	}
}

func TestToolDefsForRegistry_InstrumentalFindingsRequiresBothStructureAndParameter(t *testing.T) {
	registry := registryOfAllProviders()
	fake := &recordingChatProvider{text: "no lookup needed"}
	sessions := ask.NewSessionStore(nil)
	asker := ask.NewAsker(fake, registry, sessions, testProviderTimeout, 20, 8, nil)

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "irrelevant question")
	require.NoError(t, err)

	tool := toolByName(t, fake.lastReq.Tools, "instrumental_findings")
	required, _ := tool.Parameters["required"].([]string)
	require.ElementsMatch(t, []string{"structure", "parameter"}, required,
		"instrumental_findings must reject a call missing either field, at the schema level")
}

func TestToolDefsForRegistry_DocumentsAndEmbeddingsRequireSearchQuery(t *testing.T) {
	registry := registryOfAllProviders()
	fake := &recordingChatProvider{text: "no lookup needed"}
	sessions := ask.NewSessionStore(nil)
	asker := ask.NewAsker(fake, registry, sessions, testProviderTimeout, 20, 8, nil)

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "irrelevant question")
	require.NoError(t, err)

	for _, name := range []string{"documents", "embeddings"} {
		tool := toolByName(t, fake.lastReq.Tools, name)
		required, _ := tool.Parameters["required"].([]string)
		require.Equal(t, []string{"searchQuery"}, required, "%s must require searchQuery", name)
	}
}

func TestToolDefsForRegistry_NoParameterProvidersHaveNoRequiredFields(t *testing.T) {
	registry := registryOfAllProviders()
	fake := &recordingChatProvider{text: "no lookup needed"}
	sessions := ask.NewSessionStore(nil)
	asker := ask.NewAsker(fake, registry, sessions, testProviderTimeout, 20, 8, nil)

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "irrelevant question")
	require.NoError(t, err)

	for _, name := range []string{"medications", "diagnoses", "procedures", "profile"} {
		tool := toolByName(t, fake.lastReq.Tools, name)
		_, hasRequired := tool.Parameters["required"]
		require.False(t, hasRequired, "%s must not require any parameter", name)
	}
}
