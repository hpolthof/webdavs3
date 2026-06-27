package s3api

import (
	"encoding/xml"
	"net/http"
)

// S3 error codes and their HTTP status mappings.
var s3ErrorStatus = map[string]int{
	"NoSuchBucket":                 http.StatusNotFound,
	"NoSuchKey":                    http.StatusNotFound,
	"NoSuchUpload":                 http.StatusNotFound,
	"BucketAlreadyExists":          http.StatusConflict,
	"BucketAlreadyOwnedByYou":      http.StatusConflict,
	"InvalidBucketName":            http.StatusBadRequest,
	"InvalidPart":                  http.StatusBadRequest,
	"QuotaExceeded":                http.StatusRequestEntityTooLarge,
	"AccessDenied":                 http.StatusForbidden,
	"InvalidAccessKeyId":           http.StatusForbidden,
	"SignatureDoesNotMatch":        http.StatusForbidden,
	"MissingSecurityHeader":        http.StatusBadRequest,
	"AuthorizationHeaderMalformed": http.StatusBadRequest,
	"InvalidArgument":              http.StatusBadRequest,
	"ServiceUnavailable":           http.StatusServiceUnavailable,
	"RequestTimeTooSkewed":         http.StatusForbidden,
	"XAmzContentSHA256Mismatch":    http.StatusBadRequest,
	"InternalError":                http.StatusInternalServerError,
}

// StatusForCode returns the HTTP status code for a given S3 error code.
// Defaults to 500 for unknown codes.
func StatusForCode(code string) int {
	if s, ok := s3ErrorStatus[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// WriteS3Error writes an S3-style XML error response.
func WriteS3Error(w http.ResponseWriter, code, message, requestID string, status int) {
	body, _ := xml.Marshal(ErrorResponse{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	})
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(body)
}
