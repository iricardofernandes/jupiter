package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
)

const docBase = "https://github.com/iricardofernandes/jupiter/blob/main/docs/api/errors.md#"

// Error is a failure the client can act on. Anything else becomes a 500 api_error that
// hides its cause.
type Error struct {
	Status  int
	Type    openapi.ErrorType
	Code    string
	Message string
	Param   string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func invalidRequest(code, param, format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: openapi.InvalidRequestError, Code: code, Param: param, Message: fmt.Sprintf(format, args...)}
}

func notFound(resource, id string) *Error {
	return &Error{
		Status: http.StatusNotFound, Type: openapi.InvalidRequestError, Code: "resource_missing", Param: "id",
		Message: fmt.Sprintf("No such %s: %q", resource, id),
	}
}

func unauthenticated(message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Type: openapi.AuthenticationError, Code: "api_key_invalid", Message: message}
}

func forbidden(scope string) *Error {
	return &Error{
		Status: http.StatusForbidden, Type: openapi.PermissionError, Code: "scope_missing",
		Message: fmt.Sprintf("This API key does not have the %q scope.", scope),
	}
}

var errInternal = &Error{
	Status: http.StatusInternalServerError, Type: openapi.ApiError, Code: "internal_error",
	Message: "Something went wrong on Jupiter's end. The request can be retried with the same Idempotency-Key.",
}

func asError(err error) (*Error, bool) {
	var apiErr *Error
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

func (e *Error) body(requestID string) openapi.ErrorResponse {
	var param *string
	if e.Param != "" {
		param = &e.Param
	}
	return openapi.ErrorResponse{Error: openapi.Error{
		Type: e.Type, Code: e.Code, Message: e.Message, Param: param,
		RequestId: requestID, DocUrl: docBase + e.Code,
	}}
}
