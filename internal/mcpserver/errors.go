package mcpserver

import (
	"encoding/json"
	"fmt"
)

// mcpError formats an error matching docs/mcp/01-overview.md §12's unified
// shape ({"code": "...", "message": "..."}) as the tool handler's returned
// error — the go-sdk's mcp.AddTool wraps a non-nil handler error into the
// CallToolResult's error content verbatim (see error_test.go in the SDK),
// so this is what a client actually receives as the error text.
func mcpError(code, format string, args ...any) error {
	body, _ := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: fmt.Sprintf(format, args...)})
	return fmt.Errorf("%s", body)
}

const (
	codeUserNotFound            = "USER_NOT_FOUND"
	codeFileNotFound            = "FILE_NOT_FOUND"
	codeDocumentNotFound        = "DOCUMENT_NOT_FOUND"
	codeDocumentAlreadyImported = "DOCUMENT_ALREADY_IMPORTED"
	codeInvalidFile             = "INVALID_FILE"
	codeFileFetchFailed         = "FILE_FETCH_FAILED"
	codeInvalidEvent            = "INVALID_EVENT"
	codePipelineFailed          = "PIPELINE_FAILED"
	codeStorageError            = "STORAGE_ERROR"
	codePlannedActionNotFound   = "PLANNED_ACTION_NOT_FOUND"
	codeDiagnosisNotFound       = "DIAGNOSIS_NOT_FOUND"
	codeMedicationNotFound      = "MEDICATION_NOT_FOUND"
	codeInvalidFormat           = "INVALID_FORMAT"
	codeInvalidFields           = "INVALID_FIELDS"
)
