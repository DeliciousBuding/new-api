package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests cover the observer status route: it is registered at exactly
// /api/relay-observer/status behind middleware.RootAuth, so Admin and User
// receive 403 even when calling the route directly, anonymous requests are
// rejected, and the Root response exposes no secrets.

func TestRelayObserverStatusRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)

	found := false
	for _, route := range engine.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/relay-observer/status" {
			found = true
		}
	}
	assert.True(t, found, "GET /api/relay-observer/status must be registered")
}

func TestRelayObserverStatusAuthMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false

	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))

	rootToken := "root-access-token-000000000000000000000"
	adminToken := "admin-access-token-00000000000000000000"
	userToken := "user-access-token-000000000000000000000"
	for _, u := range []*model.User{
		{Username: "root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AccessToken: &rootToken, AffCode: "r000"},
		{Username: "admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AccessToken: &adminToken, AffCode: "a000"},
		{Username: "user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccessToken: &userToken, AffCode: "u000"},
	} {
		require.NoError(t, db.Create(u).Error)
	}

	engine := gin.New()
	SetApiRouter(engine)

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{name: "root succeeds", token: rootToken, want: http.StatusOK},
		{name: "admin forbidden", token: adminToken, want: http.StatusForbidden},
		{name: "user forbidden", token: userToken, want: http.StatusForbidden},
		{name: "anonymous unauthorized", token: "", want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/relay-observer/status", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			assert.Equal(t, tt.want, rec.Code)
		})
	}

	// The Root response carries only the safe in-memory status: no secrets.
	req := httptest.NewRequest(http.MethodGet, "/api/relay-observer/status", nil)
	req.Header.Set("Authorization", "Bearer "+rootToken)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, rootToken)
	assert.NotContains(t, body, "mysql://")
	assert.NotContains(t, body, "postgres://")
}
