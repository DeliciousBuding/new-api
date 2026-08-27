package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func resetInvitationCodes(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&InvitationCode{}))
	require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Unscoped().Delete(&InvitationCode{}).Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Unscoped().Delete(&InvitationCode{}).Error)
	})
}

func TestConsumeInvitationCodeGuards(t *testing.T) {
	resetInvitationCodes(t)
	now := common.GetTimestamp()
	codes := []InvitationCode{
		{Id: 1, Code: "AAAAAA", Status: common.InvitationCodeStatusEnabled, MaxUses: 2, UsedCount: 0, ExpiredTime: 0},
		{Id: 2, Code: "BBBBBB", Status: common.InvitationCodeStatusDisabled, MaxUses: 1, UsedCount: 0, ExpiredTime: 0},
		{Id: 3, Code: "CCCCCC", Status: common.InvitationCodeStatusEnabled, MaxUses: 1, UsedCount: 0, ExpiredTime: now - 10},
		{Id: 4, Code: "DDDDDD", Status: common.InvitationCodeStatusEnabled, MaxUses: 1, UsedCount: 1, ExpiredTime: 0},
	}
	require.NoError(t, DB.Create(&codes).Error)

	// input normalization: lower case + spaces still hit the row
	require.NoError(t, ConsumeInvitationCode(" aaaaaa "))

	assert.ErrorIs(t, ConsumeInvitationCode(""), ErrInvitationCodeRequired)
	assert.ErrorIs(t, ConsumeInvitationCode("ZZZZZZ"), ErrInvitationCodeInvalid)
	assert.ErrorIs(t, ConsumeInvitationCode("BBBBBB"), ErrInvitationCodeDisabled)
	assert.ErrorIs(t, ConsumeInvitationCode("CCCCCC"), ErrInvitationCodeExpired)
	assert.ErrorIs(t, ConsumeInvitationCode("DDDDDD"), ErrInvitationCodeExhausted)

	// second use of AAAAAA succeeds, third hits max_uses
	require.NoError(t, ConsumeInvitationCode("AAAAAA"))
	assert.ErrorIs(t, ConsumeInvitationCode("AAAAAA"), ErrInvitationCodeExhausted)

	// refund gives exactly one use back, never below zero
	RefundInvitationCodeUse("AAAAAA")
	require.NoError(t, ConsumeInvitationCode("AAAAAA"))
	assert.ErrorIs(t, ConsumeInvitationCode("AAAAAA"), ErrInvitationCodeExhausted)
}

func TestConsumeInvitationCodeConcurrencyNeverOversells(t *testing.T) {
	resetInvitationCodes(t)
	require.NoError(t, DB.Create(&InvitationCode{
		Id: 1, Code: "RACEOK", Status: common.InvitationCodeStatusEnabled, MaxUses: 3, UsedCount: 0,
	}).Error)

	const workers = 32
	var wg sync.WaitGroup
	successes := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			successes <- ConsumeInvitationCode("RACEOK")
		}()
	}
	wg.Wait()
	close(successes)

	ok := 0
	for err := range successes {
		if err == nil {
			ok++
		} else {
			assert.ErrorIs(t, err, ErrInvitationCodeExhausted)
		}
	}
	assert.Equal(t, 3, ok)

	code, err := GetInvitationCodeById(1)
	require.NoError(t, err)
	assert.Equal(t, 3, code.UsedCount)
}

func TestSearchInvitationCodesFiltersAndPaginates(t *testing.T) {
	resetInvitationCodes(t)
	now := common.GetTimestamp()
	codes := []InvitationCode{
		{Id: 1, Code: "AAA111", Name: "alpha-available", Status: common.InvitationCodeStatusEnabled, MaxUses: 2, UsedCount: 1, ExpiredTime: 0},
		{Id: 2, Code: "AAA222", Name: "alpha-expired", Status: common.InvitationCodeStatusEnabled, MaxUses: 1, UsedCount: 0, ExpiredTime: now - 10},
		{Id: 3, Code: "BBB333", Name: "beta-exhausted", Status: common.InvitationCodeStatusEnabled, MaxUses: 1, UsedCount: 1, ExpiredTime: 0},
		{Id: 4, Code: "BBB444", Name: "beta-disabled", Status: common.InvitationCodeStatusDisabled, MaxUses: 1, UsedCount: 0, ExpiredTime: 0},
	}
	require.NoError(t, DB.Create(&codes).Error)

	tests := []struct {
		name      string
		keyword   string
		status    string
		num       int
		wantTotal int64
		wantIds   []int
	}{
		{name: "no filters", num: 10, wantTotal: 4, wantIds: []int{4, 3, 2, 1}},
		{name: "keyword by name prefix", keyword: "alpha", num: 10, wantTotal: 2, wantIds: []int{2, 1}},
		{name: "keyword by exact code lower-cased", keyword: "bbb333", num: 10, wantTotal: 1, wantIds: []int{3}},
		{name: "status available", status: "available", num: 10, wantTotal: 1, wantIds: []int{1}},
		{name: "status expired", status: "expired", num: 10, wantTotal: 1, wantIds: []int{2}},
		{name: "status exhausted", status: "exhausted", num: 10, wantTotal: 1, wantIds: []int{3}},
		{name: "status disabled", status: "2", num: 10, wantTotal: 1, wantIds: []int{4}},
		{name: "pagination", num: 2, wantTotal: 4, wantIds: []int{4, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, total, err := SearchInvitationCodes(tc.keyword, tc.status, 0, tc.num)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTotal, total)
			ids := make([]int, 0, len(got))
			for _, c := range got {
				ids = append(ids, c.Id)
			}
			assert.Equal(t, tc.wantIds, ids)
		})
	}
}

func TestGenerateInvitationCodeShapeAndUniqueness(t *testing.T) {
	resetInvitationCodes(t)
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		code, err := GenerateInvitationCode()
		require.NoError(t, err)
		assert.Len(t, code, 6)
		for _, r := range code {
			assert.True(t, r >= 'A' && r <= 'Z', "code must be uppercase A-Z")
		}
		assert.False(t, seen[code], "unexpected collision in 200 samples")
		seen[code] = true
	}
}
