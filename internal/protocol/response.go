// Package protocol implements Feishu Open API and event wire primitives.
package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Code identifies a Feishu Open API result.
type Code int

// Common Feishu Open API result codes.
const (
	Success            Code = 0        // Successful request.
	InvalidParameter   Code = 10003    // Missing or invalid request parameter.
	AppUnauthorized    Code = 10014    // App is unavailable.
	WrongAppSecret     Code = 10015    // App secret does not match.
	InvalidOAuthCode   Code = 20003    // Authorization code is invalid or already used.
	ExpiredOAuthCode   Code = 20004    // Authorization code has expired.
	IMInvalidParameter Code = 230001   // An IM parameter is invalid.
	IMBotOutsideChat   Code = 230002   // The bot is not a member of the chat.
	ResourceNotFound   Code = 99991201 // Unknown Open API resource.
	RateLimited        Code = 99991400 // Request frequency limit triggered.
	InvalidToken       Code = 99991661 // Missing Bearer credential or wrong scheme.
	ExpiredTenantToken Code = 99991663 // Tenant credential is expired or unsupported.
	InvalidAppToken    Code = 99991664 // App credential is invalid.
	InvalidTenantToken Code = 99991665 // Tenant credential is unknown or illegal.
	InvalidUserToken   Code = 99991668 // User credential is invalid or unsupported.
	InvalidTokenFormat Code = 99991671 // Bearer credential format is invalid.
	InsufficientScope  Code = 99991672 // Required API permission is missing.
	FieldValidation    Code = 99992402 // Request fields failed validation.
)

// Envelope is the JSON shape shared by Feishu Open API responses.
type Envelope struct {
	Code  Code          `json:"code"`
	Msg   string        `json:"msg"`
	Data  any           `json:"data,omitempty"`
	Error *ErrorDetails `json:"error,omitempty"`
}

// ErrorDetails contains optional diagnostic information from Feishu.
type ErrorDetails struct {
	Message              string                `json:"message,omitempty"`
	Helps                []ErrorHelp           `json:"helps,omitempty"`
	LogID                string                `json:"log_id,omitempty"`
	Troubleshooter       string                `json:"troubleshooter,omitempty"`
	FieldViolations      []FieldViolation      `json:"field_violations,omitempty"`
	PermissionViolations []PermissionViolation `json:"permission_violations,omitempty"`
}

// ErrorHelp links to an action or explanation for the API error.
type ErrorHelp struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

// FieldViolation identifies an invalid request field.
type FieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// PermissionViolation identifies a missing permission.
type PermissionViolation struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
}

// APIError pairs a Feishu error envelope with its HTTP status.
type APIError struct {
	Code    Code
	Message string
	Status  int
	Details *ErrorDetails
}

type codeInfo struct {
	status int
	msg    string
}

var codes = map[Code]codeInfo{
	Success:            {http.StatusOK, "ok"},
	InvalidParameter:   {http.StatusOK, "invalid parameter"},
	AppUnauthorized:    {http.StatusOK, "app unauthorized"},
	WrongAppSecret:     {http.StatusOK, "wrong app secret"},
	InvalidOAuthCode:   {http.StatusOK, "invalid request with code"},
	ExpiredOAuthCode:   {http.StatusOK, "expired code"},
	IMInvalidParameter: {http.StatusBadRequest, "Your request contains an invalid request parameter."},
	IMBotOutsideChat:   {http.StatusBadRequest, "The bot can not be outside the group."},
	ResourceNotFound:   {http.StatusNotFound, "resource not find"},
	RateLimited:        {http.StatusTooManyRequests, "request trigger frequency limit"},
	InvalidToken:       {http.StatusBadRequest, "Missing access token for authorization. Please make a request with token attached."},
	ExpiredTenantToken: {http.StatusBadRequest, "Invalid access token for authorization. Please make a request with token attached."},
	InvalidAppToken:    {http.StatusBadRequest, "invalid app token"},
	InvalidTenantToken: {http.StatusBadRequest, "invalid tenant code"},
	InvalidUserToken:   {http.StatusBadRequest, "Invalid access token for authorization. Please make a request with token attached."},
	InvalidTokenFormat: {http.StatusBadRequest, "Invalid token: must start with t-/u-"},
	InsufficientScope:  {http.StatusForbidden, "Access denied. One of the following scopes is required"},
	FieldValidation:    {http.StatusBadRequest, "field validation failed"},
}

// NewError constructs a Feishu API error. Individual endpoints can override
// Status or Message when their documented response differs from the defaults.
func NewError(code Code, details *ErrorDetails) APIError {
	info, ok := codes[code]
	if !ok {
		info = codeInfo{http.StatusInternalServerError, "internal server error"}
	}
	return APIError{Code: code, Message: info.msg, Status: info.status, Details: details}
}

// Error implements error.
func (e APIError) Error() string {
	return fmt.Sprintf("Feishu error %d: %s", e.Code, e.Message)
}

// WriteError writes the Feishu error envelope and status to w.
func WriteError(w http.ResponseWriter, apiErr APIError) error {
	return WriteJSON(w, apiErr.Status, Envelope{Code: apiErr.Code, Msg: apiErr.Message, Error: apiErr.Details})
}

// WriteSuccess writes a successful Feishu response with a data object by default.
func WriteSuccess(w http.ResponseWriter, data any) error {
	if data == nil {
		data = struct{}{}
	}
	return WriteJSON(w, http.StatusOK, Envelope{Code: Success, Msg: "ok", Data: data})
}

// WriteResult writes a Feishu response with its code exposed to the audit writer.
func WriteResult(w http.ResponseWriter, status int, code Code, data any) error {
	return writeJSON(w, status, data, &code)
}

// WriteJSON marshals a JSON response before committing its HTTP status.
func WriteJSON(w http.ResponseWriter, status int, data any) error {
	var code *Code
	switch envelope := data.(type) {
	case Envelope:
		code = &envelope.Code
	case *Envelope:
		if envelope != nil {
			code = &envelope.Code
		}
	}
	return writeJSON(w, status, data, code)
}

func writeJSON(w http.ResponseWriter, status int, data any, code *Code) error {
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	if code != nil {
		setFeishuCode(w, *code)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

// setFeishuCode passes envelope metadata through response writer wrappers
// without exposing it as an HTTP header.
func setFeishuCode(w http.ResponseWriter, code Code) {
	for {
		switch writer := w.(type) {
		case interface{ SetFeishuCode(Code) }:
			writer.SetFeishuCode(code)
			return
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return
		}
	}
}
