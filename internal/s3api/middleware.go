package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hpolthof/webdav3s/internal/auth"
	"github.com/hpolthof/webdav3s/internal/meta"
)

type contextKey int

const (
	ctxRequestID contextKey = iota
	ctxUser
	ctxSigningKey
)

// sigContext holds information needed to verify aws-chunked streaming signatures.
type sigContext struct {
	signingKey      []byte
	seedSignature   string
	credentialScope string
	amzDate         string
}

// sigCtxFromCtx returns the streaming-signature context set by authMiddleware.
func sigCtxFromCtx(ctx context.Context) (sigContext, bool) {
	v, ok := ctx.Value(ctxSigningKey).(sigContext)
	return v, ok
}

// requestIDMiddleware injects a random request ID into the context and response header.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [16]byte
		rand.Read(b[:])
		reqID := hex.EncodeToString(b[:])
		w.Header().Set("X-Amz-Request-Id", reqID)
		r = r.WithContext(context.WithValue(r.Context(), ctxRequestID, reqID))
		next.ServeHTTP(w, r)
	})
}

// requestIDFromCtx retrieves the request ID from the context.
func requestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return "unknown"
}

// userFromCtx retrieves the authenticated User from the context.
func userFromCtx(ctx context.Context) meta.User {
	if u, ok := ctx.Value(ctxUser).(meta.User); ok {
		return u
	}
	return meta.User{}
}

// authMiddleware verifies the AWS Signature V4 and injects the user into context.
func authMiddleware(deps S3Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := requestIDFromCtx(r.Context())

			var secretKey string
			accessKey, err := deps.Auth.Verify(r, func(ak string) (string, error) {
				user, err := deps.Structure.GetUserByAccessKey(ak)
				if err != nil {
					return "", err
				}
				// Decrypt the stored secret key to obtain the plaintext required
				// for SigV4 HMAC verification.
				if deps.DecryptFn != nil && user.SecretKeyEnc != "" {
					secret, decErr := deps.DecryptFn(user.SecretKeyEnc)
					if decErr != nil {
						slog.Warn("failed to decrypt user secret key", "access_key", ak, "err", decErr)
						return "", decErr
					}
					secretKey = secret
					return secret, nil
				}
				// Fallback: return hash (broken for new users, kept for migration).
				secretKey = user.SecretKeyHash
				return user.SecretKeyHash, nil
			})
			if err != nil {
				authHeader := r.Header.Get("Authorization")
				if len(authHeader) > 120 {
					authHeader = authHeader[:120] + "..."
				}
				slog.Warn("s3 auth failed",
					"request_id", reqID,
					"access_key", accessKey,
					"method", r.Method,
					"path", r.URL.Path,
					"authorization", authHeader,
					"err", err,
				)

				switch {
				case errors.Is(err, auth.ErrMissingAuthHeader):
					WriteS3Error(w, "MissingSecurityHeader", "Missing Authorization header.", reqID, http.StatusBadRequest)
				case errors.Is(err, auth.ErrInvalidAuthHeader):
					WriteS3Error(w, "AuthorizationHeaderMalformed", err.Error(), reqID, http.StatusBadRequest)
				case errors.Is(err, auth.ErrSignatureMismatch):
					WriteS3Error(w, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your secret access key and signing method.", reqID, http.StatusForbidden)
				case errors.Is(err, auth.ErrRequestExpired):
					WriteS3Error(w, "RequestTimeTooSkewed", err.Error(), reqID, http.StatusForbidden)
				case errors.Is(err, auth.ErrContentHashMismatch):
					WriteS3Error(w, "XAmzContentSHA256Mismatch", err.Error(), reqID, http.StatusBadRequest)
				default:
					WriteS3Error(w, "InvalidAccessKeyId", "The AWS access key ID you provided does not exist.", reqID, http.StatusForbidden)
				}
				return
			}

			user, err := deps.Structure.GetUserByAccessKey(accessKey)
			if err != nil {
				slog.Warn("s3 auth user lookup failed after verify",
					"request_id", reqID,
					"access_key", accessKey,
					"err", err,
				)
				WriteS3Error(w, "InvalidAccessKeyId", "Access key not found.", reqID, http.StatusForbidden)
				return
			}
			if !user.Enabled {
				slog.Warn("s3 auth denied disabled user", "request_id", reqID, "access_key", accessKey)
				WriteS3Error(w, "AccessDenied", "User account is disabled.", reqID, http.StatusForbidden)
				return
			}

			slog.Debug("s3 auth succeeded", "request_id", reqID, "access_key", accessKey, "user_id", user.ID)
			ctx := context.WithValue(r.Context(), ctxUser, user)
			// Derive the signing key needed for aws-chunked streaming verification.
			if authHeader := r.Header.Get("Authorization"); authHeader != "" && secretKey != "" {
				if cred, _, seedSig, perr := auth.ParseAuthHeader(authHeader); perr == nil {
					parts := strings.SplitN(cred, "/", 5)
					if len(parts) == 5 {
						date, region, service := parts[1], parts[2], parts[3]
						ctx = context.WithValue(ctx, ctxSigningKey, sigContext{
							signingKey:      auth.DeriveSigningKey(secretKey, date, region, service),
							seedSignature:   seedSig,
							credentialScope: strings.Join([]string{date, region, service, "aws4_request"}, "/"),
							amzDate:         r.Header.Get("x-amz-date"),
						})
					}
				}
			}
			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		})
	}
}

// chain applies middleware in order (first middleware is outermost).
func chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
