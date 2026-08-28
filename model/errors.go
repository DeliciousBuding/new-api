package model

import "errors"

// Common errors
var (
	ErrDatabase = errors.New("database error")
)

// User auth errors
var (
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrUserEmptyCredentials = errors.New("empty credentials")
	ErrEmailAlreadyTaken    = errors.New("email already taken")
	ErrEmailNotFound        = errors.New("email not found")
	ErrEmailAmbiguous       = errors.New("email matches multiple users")
)

// Token auth errors
var (
	ErrTokenNotProvided = errors.New("token not provided")
	ErrTokenInvalid     = errors.New("token invalid")
)

// Redemption errors
var ErrRedeemFailed = errors.New("redeem.failed")

// Invitation code errors
var (
	ErrInvitationCodeRequired  = errors.New("invitation code is required")
	ErrInvitationCodeInvalid   = errors.New("invalid invitation code")
	ErrInvitationCodeDisabled  = errors.New("invitation code is disabled")
	ErrInvitationCodeExpired   = errors.New("invitation code is expired")
	ErrInvitationCodeExhausted = errors.New("invitation code has reached its max uses")
)

// 2FA errors
var ErrTwoFANotEnabled = errors.New("2fa not enabled")
var ErrTwoFAAlreadyEnabled = errors.New("2fa already enabled")
