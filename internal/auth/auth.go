// Package auth checks API keys. Every /api/v1 request must carry
// "Authorization: Bearer <key>"; the key identifies the user, and the
// handlers only ever touch that user's apps.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/gin-gonic/gin"
)

// "onv_" makes a leaked key recognizable at a glance, and lets secret
// scanners (GitHub's, for example) be taught to spot it in public code.
const prefix = "onv_"

const userIDKey = "userID"

// NewToken returns a new API key and the hash to store. The key itself is
// shown once and never saved.
func NewToken() (token string, hash []byte, err error) {
	b := make([]byte, 32) // 256 bits: unguessable, so no rate limit needed
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = prefix + hex.EncodeToString(b)
	return token, Hash(token), nil
}

func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type Lookup interface {
	UserIDByTokenHash(ctx context.Context, hash []byte) (string, error)
}

// Middleware rejects requests without a valid key and records the user's
// ID for the handlers. The key is compared by looking up its hash, so the
// secret itself is never compared byte by byte (no timing leak).
func Middleware(db Lookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(token, prefix) {
			unauthorized(c, "Send your API key as: Authorization: Bearer onv_...")
			return
		}
		userID, err := db.UserIDByTokenHash(c.Request.Context(), Hash(token))
		if errors.Is(err, store.ErrInvalidToken) {
			unauthorized(c, "Invalid API key")
			return
		}
		if err != nil {
			log.Printf("auth: look up token: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Could not check your API key"})
			return
		}
		c.Set(userIDKey, userID)
		c.Next()
	}
}

// UserID returns the authenticated user's ID. Only valid behind Middleware.
func UserID(c *gin.Context) string { return c.GetString(userIDKey) }

func unauthorized(c *gin.Context, msg string) {
	// WWW-Authenticate tells clients which scheme to use, as HTTP requires for 401.
	c.Header("WWW-Authenticate", "Bearer")
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
}
