package controller

import (
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func GetAllInvitationCodes(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	invitationCodes, total, err := model.GetAllInvitationCodes(pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(invitationCodes)
	common.ApiSuccess(c, pageInfo)
	return
}

func SearchInvitationCodes(c *gin.Context) {
	keyword := c.Query("keyword")
	status := c.Query("status")
	pageInfo := common.GetPageQuery(c)
	invitationCodes, total, err := model.SearchInvitationCodes(keyword, status, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(invitationCodes)
	common.ApiSuccess(c, pageInfo)
	return
}

func GetInvitationCode(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	invitationCode, err := model.GetInvitationCodeById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    invitationCode,
	})
	return
}

func AddInvitationCode(c *gin.Context) {
	invitationCode := model.InvitationCode{}
	err := c.ShouldBindJSON(&invitationCode)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if utf8.RuneCountInString(invitationCode.Name) == 0 || utf8.RuneCountInString(invitationCode.Name) > 20 {
		common.ApiErrorI18n(c, i18n.MsgInvitationCodeNameLength)
		return
	}
	if invitationCode.Count <= 0 {
		common.ApiErrorI18n(c, i18n.MsgInvitationCodeCountPositive)
		return
	}
	if invitationCode.Count > 100 {
		common.ApiErrorI18n(c, i18n.MsgInvitationCodeCountMax)
		return
	}
	if invitationCode.MaxUses <= 0 {
		common.ApiErrorI18n(c, i18n.MsgInvitationCodeMaxUsesPositive)
		return
	}
	if valid, msg := validateExpiredTime(c, invitationCode.ExpiredTime); !valid {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": msg})
		return
	}
	var codes []string
	for i := 0; i < invitationCode.Count; i++ {
		code, err := model.GenerateUnusedInvitationCode()
		if err != nil {
			common.SysError("failed to generate invitation code: " + err.Error())
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": i18n.T(c, i18n.MsgInvitationCodeCreateFailed),
				"data":    codes,
			})
			return
		}
		cleanInvitationCode := model.InvitationCode{
			Code:        code,
			Name:        invitationCode.Name,
			MaxUses:     invitationCode.MaxUses,
			CreatedTime: common.GetTimestamp(),
			ExpiredTime: invitationCode.ExpiredTime,
		}
		if err := cleanInvitationCode.Insert(); err != nil {
			common.SysError("failed to insert invitation code: " + err.Error())
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": i18n.T(c, i18n.MsgInvitationCodeCreateFailed),
				"data":    codes,
			})
			return
		}
		codes = append(codes, cleanInvitationCode.Code)
	}
	recordManageAudit(c, "invitation_code.create", map[string]interface{}{
		"name":     invitationCode.Name,
		"count":    invitationCode.Count,
		"max_uses": invitationCode.MaxUses,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    codes,
	})
	return
}

func DeleteInvitationCode(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := model.DeleteInvitationCodeById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

func UpdateInvitationCode(c *gin.Context) {
	statusOnly := c.Query("status_only")
	invitationCode := model.InvitationCode{}
	err := c.ShouldBindJSON(&invitationCode)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	cleanInvitationCode, err := model.GetInvitationCodeById(invitationCode.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if statusOnly == "" {
		if invitationCode.MaxUses <= 0 {
			common.ApiErrorI18n(c, i18n.MsgInvitationCodeMaxUsesPositive)
			return
		}
		if valid, msg := validateExpiredTime(c, invitationCode.ExpiredTime); !valid {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": msg})
			return
		}
		// If you add more fields, please also update InvitationCode.Update()
		cleanInvitationCode.Name = invitationCode.Name
		cleanInvitationCode.MaxUses = invitationCode.MaxUses
		cleanInvitationCode.ExpiredTime = invitationCode.ExpiredTime
	}
	if statusOnly != "" {
		cleanInvitationCode.Status = invitationCode.Status
	}
	err = cleanInvitationCode.Update()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    cleanInvitationCode,
	})
	return
}

func DeleteInvalidInvitationCode(c *gin.Context) {
	rows, err := model.DeleteInvalidInvitationCodes()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    rows,
	})
	return
}
