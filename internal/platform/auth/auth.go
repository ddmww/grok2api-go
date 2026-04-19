package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/gin-gonic/gin"
)

func bearerToken(header string) string {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func APIKey(cfg *config.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(cfg.GetString("app.api_key", ""))
		if raw == "" {
			c.Next()
			return
		}
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "Missing or invalid Authorization header.", "type": "invalid_request_error"}})
			return
		}
		for _, key := range strings.Split(raw, ",") {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(key)), []byte(token)) == 1 {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "Invalid API key.", "type": "invalid_request_error"}})
	}
}

func AdminKey(cfg *config.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := cfg.GetString("app.app_key", "grok2api")
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			token = c.Query("app_key")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(key), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"message": "Invalid authentication token."})
			return
		}
		c.Next()
	}
}
