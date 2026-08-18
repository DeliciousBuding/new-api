package service

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelSelectAutoGroupsTest(t *testing.T) *gorm.DB {
	t.Helper()

	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	originalMaxTokenAutoGroups := setting.GetMaxTokenAutoGroups()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.MemoryCacheEnabled = true
	common.RetryTimes = 0

	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("2"))

	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.RetryTimes = originalRetryTimes
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMaxTokenAutoGroups)))

		if originalMemoryCacheEnabled && originalDB != nil &&
			originalDB.Migrator().HasTable(&model.Channel{}) && originalDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		if err == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	return db
}

func createChannelSelectAutoGroupsChannel(t *testing.T, db *gorm.DB, id int, group, modelName string) {
	t.Helper()
	priority := int64(0)
	weight := uint(100)
	require.NoError(t, db.Create(&model.Channel{
		Id:       id,
		Type:     constant.ChannelTypeOpenAI,
		Key:      fmt.Sprintf("key-%d", id),
		Status:   common.ChannelStatusEnabled,
		Name:     fmt.Sprintf("channel-%d", id),
		Weight:   &weight,
		Models:   modelName,
		Group:    group,
		Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     group,
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func TestUpstreamAttemptBudgetSurvivesAutoGroupSwitch(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "auto-groups-budget-model"
	createChannelSelectAutoGroupsChannel(t, db, 2201, "vip", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 2202, "default", modelName)
	model.InitChannelCache()

	originalMax := common.MaxUpstreamAttempts
	t.Cleanup(func() { common.MaxUpstreamAttempts = originalMax })

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	common.SetContextKey(ctx, constant.ContextKeyTokenCrossGroupRetry, true)

	retry := 0
	param := &RetryParam{
		Ctx:         ctx,
		TokenGroup:  "auto",
		ModelName:   modelName,
		RequestPath: "/v1/chat/completions",
		Retry:       &retry,
	}

	common.MaxUpstreamAttempts = 0
	first, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	param.IncreaseAttempts()

	param.IncreaseRetry()
	second, secondGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	param.IncreaseAttempts()
	require.Equal(t, "default", secondGroup, "expected the selection to move to the next auto group")

	// This is the whole point: crossing an auto group resets Retry (it doubles as
	// the per-group priority tier index), which is why RetryTimes alone cannot
	// bound total upstream attempts.
	assert.Equal(t, 0, param.GetRetry(), "auto-group switch is expected to reset the per-group retry counter")
	assert.Equal(t, 2, param.Attempts(), "global attempt count must not be reset by the group switch")

	// Cap of 0 keeps the historical unbounded behaviour.
	common.MaxUpstreamAttempts = 0
	assert.False(t, param.UpstreamBudgetExhausted())

	// A cap that the request already reached stops further retries even though
	// GetRetry() is back at 0 and would still satisfy the RetryTimes loop.
	common.MaxUpstreamAttempts = 2
	assert.True(t, param.UpstreamBudgetExhausted())

	common.MaxUpstreamAttempts = 4
	assert.False(t, param.UpstreamBudgetExhausted())
}

func TestUpstreamBudgetExhaustedBoundaries(t *testing.T) {
	originalMax := common.MaxUpstreamAttempts
	t.Cleanup(func() { common.MaxUpstreamAttempts = originalMax })

	param := &RetryParam{}
	assert.Equal(t, 0, param.Attempts())

	// Unlimited (default) and negative caps must never bound the request.
	for _, cap := range []int{0, -1} {
		common.MaxUpstreamAttempts = cap
		param.attempts = 100
		assert.False(t, param.UpstreamBudgetExhausted(), "cap %d must mean unlimited", cap)
	}

	// A cap of 1 means the first attempt is the only one allowed.
	common.MaxUpstreamAttempts = 1
	param.attempts = 0
	assert.False(t, param.UpstreamBudgetExhausted(), "budget must not be exhausted before the first attempt")
	param.IncreaseAttempts()
	assert.True(t, param.UpstreamBudgetExhausted(), "cap 1 must forbid a second attempt")
}

func TestCacheGetRandomSatisfiedChannelUsesTokenAutoGroupsWhenGlobalAutoIsEmpty(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "auto-groups-runtime-model"
	createChannelSelectAutoGroupsChannel(t, db, 2101, "vip", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 2102, "default", modelName)
	model.InitChannelCache()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	common.SetContextKey(ctx, constant.ContextKeyTokenCrossGroupRetry, true)

	retry := 0
	param := &RetryParam{
		Ctx:         ctx,
		TokenGroup:  "auto",
		ModelName:   modelName,
		RequestPath: "/v1/chat/completions",
		Retry:       &retry,
	}

	first, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 2101, first.Id)
	assert.Equal(t, "vip", selectedGroup)
	assert.Equal(t, "vip", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
	assert.Empty(t, setting.GetAutoGroups(), "the selection must not depend on the global Auto list")

	param.IncreaseRetry()
	second, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 2102, second.Id)
	assert.Equal(t, "default", selectedGroup)
	assert.Equal(t, "default", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
}
