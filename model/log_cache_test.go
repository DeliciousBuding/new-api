package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
)

func TestCacheDayBucketExprPerDatabase(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})

	// MySQL 的 `/` 返回 decimal（5/2=2.5000），必须用 DIV 才得到整数天桶
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
	assert.Equal(t, "(created_at DIV 86400)", cacheDayBucketExpr())

	// ClickHouse 的 `/` 是浮点除法，必须用 intDiv
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse)
	assert.Equal(t, "intDiv(created_at, 86400)", cacheDayBucketExpr())

	// PostgreSQL / SQLite 的 int/int 即为整除
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypePostgreSQL)
	assert.Equal(t, "(created_at / 86400)", cacheDayBucketExpr())

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	assert.Equal(t, "(created_at / 86400)", cacheDayBucketExpr())
}

func TestCacheJsonExtractExprPerDatabase(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypePostgreSQL)
	assert.Equal(t, "COALESCE((other::jsonb->>'cache_tokens')::bigint, 0)", cacheJsonExtractExpr("cache_tokens"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
	assert.Equal(t, "COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(other, '$.cache_tokens')) AS SIGNED), 0)", cacheJsonExtractExpr("cache_tokens"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse)
	assert.Equal(t, "JSONExtractInt(other, 'cache_tokens')", cacheJsonExtractExpr("cache_tokens"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	assert.Equal(t, "COALESCE(CAST(json_extract(other, '$.cache_tokens') AS INTEGER), 0)", cacheJsonExtractExpr("cache_tokens"))
}

func TestCacheJsonTextExtractExprPerDatabase(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypePostgreSQL)
	assert.Equal(t, "(other::jsonb->>'usage_semantic')", cacheJsonTextExtractExpr("usage_semantic"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
	assert.Equal(t, "JSON_UNQUOTE(JSON_EXTRACT(other, '$.usage_semantic'))", cacheJsonTextExtractExpr("usage_semantic"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse)
	assert.Equal(t, "JSONExtractString(other, 'usage_semantic')", cacheJsonTextExtractExpr("usage_semantic"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	assert.Equal(t, "json_extract(other, '$.usage_semantic')", cacheJsonTextExtractExpr("usage_semantic"))
}

func TestCacheJsonBoolExtractExprPerDatabase(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypePostgreSQL)
	assert.Equal(t, "COALESCE((other::jsonb->>'claude')::boolean, false)", cacheJsonBoolExtractExpr("claude"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
	assert.Equal(t, "COALESCE(JSON_EXTRACT(other, '$.claude') = true, false)", cacheJsonBoolExtractExpr("claude"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse)
	assert.Equal(t, "JSONExtractBool(other, 'claude')", cacheJsonBoolExtractExpr("claude"))

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	assert.Equal(t, "COALESCE(CAST(json_extract(other, '$.claude') AS INTEGER), 0)", cacheJsonBoolExtractExpr("claude"))
}

func TestCacheRateInputExprIncludesProtocolSemantics(t *testing.T) {
	t.Cleanup(func() {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	})
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	expr := cacheRateInputExpr()
	assert.Contains(t, expr, "usage_semantic")
	assert.Contains(t, expr, "= 'anthropic'")
	assert.Contains(t, expr, "claude")
	assert.Contains(t, expr, "cache_tokens")
	assert.Contains(t, expr, "cache_creation_tokens")
	assert.Contains(t, expr, "input_tokens_total")
	assert.Contains(t, expr, "prompt_tokens")
}
