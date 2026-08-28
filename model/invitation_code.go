package model

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// InvitationCode gates user registration. It is intentionally separate from
// Redemption: redemption codes carry quota and are consumed by logged-in
// users on the top-up page, while invitation codes grant permission to
// register and are consumed atomically when a new account is created.
type InvitationCode struct {
	Id          int            `json:"id"`
	Code        string         `json:"code" gorm:"type:char(6);uniqueIndex"`
	Status      int            `json:"status" gorm:"default:1"`
	Name        string         `json:"name" gorm:"index"`
	MaxUses     int            `json:"max_uses" gorm:"default:1"`
	UsedCount   int            `json:"used_count" gorm:"default:0"`
	CreatedTime int64          `json:"created_time" gorm:"bigint"`
	ExpiredTime int64          `json:"expired_time" gorm:"bigint"` // 0 means never expires
	Count       int            `json:"count" gorm:"-:all"`         // only for api request: how many codes to generate
	DeletedAt   gorm.DeletedAt `gorm:"index"`
}

const invitationCodeCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// GenerateInvitationCode returns a random 6-char uppercase code.
// crypto/rand keeps generation unpredictable; rejection-free big.Int sampling
// avoids modulo bias across the 26-letter charset.
func GenerateInvitationCode() (string, error) {
	var sb strings.Builder
	for i := 0; i < 6; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(invitationCodeCharset))))
		if err != nil {
			return "", err
		}
		sb.WriteByte(invitationCodeCharset[n.Int64()])
	}
	return sb.String(), nil
}

// GenerateUnusedInvitationCode generates a code that no existing row uses.
// The pre-check keeps collision handling dialect-agnostic; the unique index
// remains the final authority and a rare race just fails the insert like
// Redemption does.
func GenerateUnusedInvitationCode() (string, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		code, err := GenerateInvitationCode()
		if err != nil {
			return "", err
		}
		var count int64
		if err := DB.Model(&InvitationCode{}).Where("code = ?", code).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return code, nil
		}
	}
	return "", errors.New("failed to generate an unused invitation code")
}

// NormalizeInvitationCode trims and upper-cases user input before any lookup.
func NormalizeInvitationCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func GetAllInvitationCodes(startIdx int, num int) (codes []*InvitationCode, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	err = tx.Model(&InvitationCode{}).Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	err = tx.Order("id desc").Limit(num).Offset(startIdx).Find(&codes).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return codes, total, nil
}

func SearchInvitationCodes(keyword string, status string, startIdx int, num int) (codes []*InvitationCode, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&InvitationCode{})

	if keyword != "" {
		keyword = strings.TrimSpace(keyword)
		if id, convErr := strconv.Atoi(keyword); convErr == nil {
			query = query.Where("id = ? OR name LIKE ? OR code = ?", id, keyword+"%", NormalizeInvitationCode(keyword))
		} else {
			query = query.Where("name LIKE ? OR code = ?", keyword+"%", NormalizeInvitationCode(keyword))
		}
	}

	if status != "" {
		now := common.GetTimestamp()
		switch status {
		case "available":
			query = query.Where(
				"status = ? AND used_count < max_uses AND (expired_time = 0 OR expired_time >= ?)",
				common.InvitationCodeStatusEnabled,
				now,
			)
		case "expired":
			query = query.Where(
				"status = ? AND expired_time != 0 AND expired_time < ?",
				common.InvitationCodeStatusEnabled,
				now,
			)
		case "exhausted":
			query = query.Where(
				"status = ? AND used_count >= max_uses",
				common.InvitationCodeStatusEnabled,
			)
		case strconv.Itoa(common.InvitationCodeStatusEnabled):
			query = query.Where("status = ?", common.InvitationCodeStatusEnabled)
		case strconv.Itoa(common.InvitationCodeStatusDisabled):
			query = query.Where("status = ?", common.InvitationCodeStatusDisabled)
		}
	}

	err = query.Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&codes).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return codes, total, nil
}

func GetInvitationCodeById(id int) (*InvitationCode, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	invitationCode := InvitationCode{Id: id}
	err := DB.First(&invitationCode, "id = ?", id).Error
	return &invitationCode, err
}

func (invitationCode *InvitationCode) Insert() error {
	if invitationCode.MaxUses <= 0 {
		return errors.New("invitation code max uses must be positive")
	}
	return DB.Create(invitationCode).Error
}

// Update persists the whitelisted fields only, mirroring Redemption.Update.
func (invitationCode *InvitationCode) Update() error {
	if invitationCode.MaxUses <= 0 {
		return errors.New("invitation code max uses must be positive")
	}
	return DB.Model(invitationCode).Select("name", "status", "max_uses", "expired_time").Updates(invitationCode).Error
}

func (invitationCode *InvitationCode) Delete() error {
	return DB.Delete(invitationCode).Error
}

func DeleteInvitationCodeById(id int) error {
	if id == 0 {
		return errors.New("id 为空！")
	}
	invitationCode := InvitationCode{Id: id}
	if err := DB.Where(invitationCode).First(&invitationCode).Error; err != nil {
		return err
	}
	return invitationCode.Delete()
}

// DeleteInvalidInvitationCodes removes disabled, expired or fully used codes.
func DeleteInvalidInvitationCodes() (int64, error) {
	now := common.GetTimestamp()
	result := DB.Where(
		"status = ? OR (status = ? AND (expired_time != 0 AND expired_time < ? OR used_count >= max_uses))",
		common.InvitationCodeStatusDisabled,
		common.InvitationCodeStatusEnabled,
		now,
	).Delete(&InvitationCode{})
	return result.RowsAffected, result.Error
}

// ConsumeInvitationCode validates the code and atomically consumes one use.
// The conditional UPDATE acts as a compare-and-swap: concurrent registrations
// with the same code cannot exceed max_uses even without a row lock, because
// only the updates that still satisfy the guards affect a row.
func ConsumeInvitationCode(code string) error {
	code = NormalizeInvitationCode(code)
	if code == "" {
		return ErrInvitationCodeRequired
	}
	invitationCode := &InvitationCode{}
	if err := DB.Where("code = ?", code).First(invitationCode).Error; err != nil {
		return ErrInvitationCodeInvalid
	}
	if invitationCode.Status != common.InvitationCodeStatusEnabled {
		return ErrInvitationCodeDisabled
	}
	if invitationCode.ExpiredTime != 0 && invitationCode.ExpiredTime < common.GetTimestamp() {
		return ErrInvitationCodeExpired
	}
	if invitationCode.UsedCount >= invitationCode.MaxUses {
		return ErrInvitationCodeExhausted
	}
	now := common.GetTimestamp()
	result := DB.Model(&InvitationCode{}).
		Where(
			"id = ? AND status = ? AND used_count < max_uses AND (expired_time = 0 OR expired_time >= ?)",
			invitationCode.Id,
			common.InvitationCodeStatusEnabled,
			now,
		).
		UpdateColumn("used_count", gorm.Expr("used_count + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// Lost the race against a concurrent consume.
		return ErrInvitationCodeExhausted
	}
	return nil
}

// RefundInvitationCodeUse gives one use back when account creation fails
// after the code was consumed. Best effort by design: failing here only loses
// one use slot (fail-closed), it can never grant an extra registration.
func RefundInvitationCodeUse(code string) {
	code = NormalizeInvitationCode(code)
	if code == "" {
		return
	}
	result := DB.Model(&InvitationCode{}).
		Where("code = ? AND used_count > 0", code).
		UpdateColumn("used_count", gorm.Expr("used_count - 1"))
	if result.Error != nil {
		common.SysError("failed to refund invitation code use: " + result.Error.Error())
	}
}
