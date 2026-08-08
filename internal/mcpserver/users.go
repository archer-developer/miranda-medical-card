package mcpserver

import (
	"strings"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

// userGate is the single source of truth every handler consults for
// docs/domain/02-user.md §4's sharing rules — config.Config.Users loaded
// once at startup (see docs/architecture/01-overview.md §4: "User не
// хранится в SQLite... источник истины — статический YAML-файл").
type userGate struct {
	users map[string]config.UserConfig
	// sharedFrom[requesterID] lists every ownerID whose config lists
	// requesterID in shared_with — precomputed once so a resource-ownership
	// check (see resolveOwnedResource) doesn't rescan every user on every
	// call.
	sharedFrom map[string][]string
}

func newUserGate(users []config.UserConfig) *userGate {
	g := &userGate{users: make(map[string]config.UserConfig, len(users)), sharedFrom: make(map[string][]string)}
	for _, u := range users {
		g.users[u.ID] = u
		for _, sharedWithID := range u.SharedWith {
			g.sharedFrom[sharedWithID] = append(g.sharedFrom[sharedWithID], u.ID)
		}
	}
	return g
}

// requireUser checks userID exists in configuration — every MCP Tool's
// first check (docs/architecture/01-overview.md §4).
func (g *userGate) requireUser(userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return mcpError(codeUserNotFound, "userId is required")
	}
	if _, ok := g.users[userID]; !ok {
		return mcpError(codeUserNotFound, "unknown userId %q", userID)
	}
	return nil
}

// resolveSubject implements docs/mcp/01-overview.md §11's subjectId
// handling for aggregated-read tools (medical.ask, medical.profile,
// medical.timeline, medical.list_documents): subjectId defaults to userID;
// if different, userID must appear in subjectID's shared_with, or the
// response is identical to subjectID not existing (never revealing that a
// user exists but access is denied).
func (g *userGate) resolveSubject(userID, subjectID string) (string, error) {
	if err := g.requireUser(userID); err != nil {
		return "", err
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" || subjectID == userID {
		return userID, nil
	}
	subject, ok := g.users[subjectID]
	if !ok {
		return "", mcpError(codeUserNotFound, "unknown userId %q", subjectID)
	}
	if !contains(subject.SharedWith, userID) {
		// Deliberately the same error as "unknown subjectId" — see this
		// method's doc comment.
		return "", mcpError(codeUserNotFound, "unknown userId %q", subjectID)
	}
	return subjectID, nil
}

// resolveOwnersToTry returns, in order, every userID a resource-scoped read
// (medical.get_document, medical.download_file) should try as the owner:
// the requester itself first (the common case, and the only one write
// operations ever use), then everyone who has shared with the requester —
// see docs/mcp/01-overview.md §11's "Tools, работающие с конкретным
// ресурсом". Repositories only expose owner-scoped lookups (Get(id,
// userID) — see e.g. storage.FileRepository.Get), so this list is tried in
// order rather than doing an unscoped lookup and checking sharing after
// the fact; the household-scale user count makes this cheap.
func (g *userGate) resolveOwnersToTry(requesterID string) []string {
	return append([]string{requesterID}, g.sharedFrom[requesterID]...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
