package service

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	codexQuotaAutoEnableAtKey         = "codex_quota_auto_enable_at"
	codexQuotaAutoEnableDisabledAtKey = "codex_quota_auto_enable_disabled_at"
	codexQuotaAutoEnableReasonKey     = "codex_quota_auto_enable_reason"
	codexQuotaAutoEnableGracePeriod   = 2 * time.Minute
)

type codexQuotaAutoEnableSchedule struct {
	channelID   int
	channelName string
	enableAt    int64
	disabledAt  int64
}

var codexQuotaAutoEnableTimers sync.Map

func scheduleCodexQuotaAutoEnableAfterDisable(channelError types.ChannelError) {
	if channelError.ChannelType != constant.ChannelTypeCodex {
		return
	}
	if !common.AutomaticEnableChannelEnabled {
		return
	}

	gopool.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), codexCredentialRefreshTimeout)
		defer cancel()
		scheduleCodexQuotaAutoEnableAfterDisableSync(ctx, channelError.ChannelId, channelError.ChannelName)
	})
}

func scheduleCodexQuotaAutoEnableAfterDisableSync(ctx context.Context, channelID int, channelName string) {
	ch, err := model.GetChannelById(channelID, true)
	if err != nil || ch == nil {
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d load channel failed: %v", channelID, err))
		}
		return
	}
	if ch.Type != constant.ChannelTypeCodex || ch.Status != common.ChannelStatusAutoDisabled || ch.ChannelInfo.IsMultiKey {
		return
	}

	oauthKey, err := parseCodexOAuthKey(strings.TrimSpace(ch.Key))
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s parse key failed: %v", ch.Id, ch.Name, err))
		return
	}

	client, err := NewProxyHttpClient(ch.GetSetting().Proxy)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s create client failed: %v", ch.Id, ch.Name, err))
		return
	}

	now := time.Now()
	statusCode, body, err := FetchCodexWhamUsage(ctx, client, ch.GetBaseURL(), strings.TrimSpace(oauthKey.AccessToken), strings.TrimSpace(oauthKey.AccountID))
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s fetch usage failed: %v", ch.Id, ch.Name, err))
		return
	}

	if (statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden) && strings.TrimSpace(oauthKey.RefreshToken) != "" {
		newKey, _, refreshErr := RefreshCodexChannelCredential(ctx, ch.Id, CodexCredentialRefreshOptions{ResetCaches: false})
		if refreshErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s refresh before usage retry failed: %v", ch.Id, ch.Name, refreshErr))
			return
		}
		model.InitChannelCache()
		ResetProxyClientCache()

		statusCode, body, err = FetchCodexWhamUsage(ctx, client, ch.GetBaseURL(), strings.TrimSpace(newKey.AccessToken), strings.TrimSpace(newKey.AccountID))
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s fetch usage after refresh failed: %v", ch.Id, ch.Name, err))
			return
		}
	}

	if statusCode < 200 || statusCode >= 300 {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s upstream status=%d", ch.Id, ch.Name, statusCode))
		return
	}

	enableAtTime, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s parse usage failed: %v", ch.Id, ch.Name, err))
		return
	}
	if !ok {
		if common.DebugEnabled {
			logger.LogDebug(ctx, "codex quota auto-enable: channel_id=%d name=%s not scheduled: %s", ch.Id, ch.Name, reason)
		}
		return
	}
	enableAtTime = enableAtTime.Add(codexQuotaAutoEnableGracePeriod)

	refreshedChannel, err := model.GetChannelById(channelID, true)
	if err != nil || refreshedChannel == nil {
		return
	}
	if refreshedChannel.Status != common.ChannelStatusAutoDisabled {
		return
	}

	info := refreshedChannel.GetOtherInfo()
	disabledAt := int64FromMapValue(info["status_time"])
	if disabledAt <= 0 {
		disabledAt = common.GetTimestamp()
		info["status_time"] = disabledAt
	}
	enableAt := enableAtTime.Unix()
	info[codexQuotaAutoEnableAtKey] = enableAt
	info[codexQuotaAutoEnableDisabledAtKey] = disabledAt
	info[codexQuotaAutoEnableReasonKey] = reason
	if err := saveCodexQuotaAutoEnableInfo(refreshedChannel, info); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s save schedule failed: %v", refreshedChannel.Id, refreshedChannel.Name, err))
		return
	}

	name := strings.TrimSpace(channelName)
	if name == "" {
		name = refreshedChannel.Name
	}
	logger.LogInfo(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s scheduled at %s: %s", refreshedChannel.Id, name, enableAtTime.Format(time.RFC3339), reason))
	scheduleCodexQuotaAutoEnableTimer(codexQuotaAutoEnableSchedule{
		channelID:   refreshedChannel.Id,
		channelName: name,
		enableAt:    enableAt,
		disabledAt:  disabledAt,
	})
}

func enableCodexChannelIfQuotaAutoEnableDue(ctx context.Context, ch *model.Channel) bool {
	if ch == nil || ch.Type != constant.ChannelTypeCodex {
		return false
	}
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if ch.Status != common.ChannelStatusAutoDisabled || ch.ChannelInfo.IsMultiKey {
		return false
	}

	info := ch.GetOtherInfo()
	schedule := codexQuotaScheduleFromInfo(ch.Id, ch.Name, info)
	if schedule.enableAt <= 0 || schedule.disabledAt <= 0 {
		return false
	}
	if int64FromMapValue(info["status_time"]) != schedule.disabledAt {
		return false
	}
	if schedule.enableAt > common.GetTimestamp() {
		scheduleCodexQuotaAutoEnableTimer(schedule)
		return false
	}

	if !enableCodexChannelFromQuotaSchedule(ctx, ch, schedule) {
		return false
	}
	codexQuotaAutoEnableTimers.Delete(schedule.channelID)
	return true
}

func recoverCodexQuotaAutoEnableWithoutSchedule(ctx context.Context, ch *model.Channel) bool {
	if ch == nil || ch.Type != constant.ChannelTypeCodex {
		return false
	}
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if ch.Status != common.ChannelStatusAutoDisabled || ch.ChannelInfo.IsMultiKey {
		return false
	}

	info := ch.GetOtherInfo()
	if !isCodexUsageLimitDisabledInfo(info) {
		return false
	}
	if int64FromMapValue(info[codexQuotaAutoEnableAtKey]) > 0 ||
		int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]) > 0 {
		return false
	}

	disabledAt := int64FromMapValue(info["status_time"])
	if disabledAt <= 0 {
		return false
	}

	oauthKey, err := parseCodexOAuthKey(strings.TrimSpace(ch.Key))
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery parse key failed: %v", ch.Id, ch.Name, err))
		return false
	}

	client, err := NewProxyHttpClient(ch.GetSetting().Proxy)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery create client failed: %v", ch.Id, ch.Name, err))
		return false
	}

	now := time.Now()
	statusCode, body, err := FetchCodexWhamUsage(ctx, client, ch.GetBaseURL(), strings.TrimSpace(oauthKey.AccessToken), strings.TrimSpace(oauthKey.AccountID))
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery fetch usage failed: %v", ch.Id, ch.Name, err))
		return false
	}

	if (statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden) && strings.TrimSpace(oauthKey.RefreshToken) != "" {
		newKey, _, refreshErr := RefreshCodexChannelCredential(ctx, ch.Id, CodexCredentialRefreshOptions{ResetCaches: false})
		if refreshErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery refresh before usage retry failed: %v", ch.Id, ch.Name, refreshErr))
			return false
		}
		model.InitChannelCache()
		ResetProxyClientCache()

		statusCode, body, err = FetchCodexWhamUsage(ctx, client, ch.GetBaseURL(), strings.TrimSpace(newKey.AccessToken), strings.TrimSpace(newKey.AccountID))
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery fetch usage after refresh failed: %v", ch.Id, ch.Name, err))
			return false
		}
	}

	if statusCode < 200 || statusCode >= 300 {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery upstream status=%d", ch.Id, ch.Name, statusCode))
		return false
	}

	available, availableReason, err := CodexWhamUsageQuotaAvailable(body)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery parse usage availability failed: %v", ch.Id, ch.Name, err))
		return false
	}
	if available {
		if enableCodexChannelFromQuotaRecovery(ctx, ch, disabledAt) {
			logger.LogInfo(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovered immediately: %s", ch.Id, ch.Name, availableReason))
			return true
		}
		return false
	}

	enableAtTime, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery parse usage schedule failed: %v", ch.Id, ch.Name, err))
		return false
	}
	if !ok {
		if common.DebugEnabled {
			logger.LogDebug(ctx, "codex quota auto-enable: channel_id=%d name=%s recovery not scheduled: %s", ch.Id, ch.Name, reason)
		}
		return false
	}

	enableAtTime = enableAtTime.Add(codexQuotaAutoEnableGracePeriod)
	refreshedChannel, err := model.GetChannelById(ch.Id, true)
	if err != nil || refreshedChannel == nil {
		return false
	}
	if refreshedChannel.Status != common.ChannelStatusAutoDisabled {
		return false
	}
	refreshedInfo := refreshedChannel.GetOtherInfo()
	if int64FromMapValue(refreshedInfo["status_time"]) != disabledAt {
		return false
	}
	if int64FromMapValue(refreshedInfo[codexQuotaAutoEnableAtKey]) > 0 ||
		int64FromMapValue(refreshedInfo[codexQuotaAutoEnableDisabledAtKey]) > 0 {
		return false
	}

	enableAt := enableAtTime.Unix()
	refreshedInfo[codexQuotaAutoEnableAtKey] = enableAt
	refreshedInfo[codexQuotaAutoEnableDisabledAtKey] = disabledAt
	refreshedInfo[codexQuotaAutoEnableReasonKey] = reason
	if err := saveCodexQuotaAutoEnableInfo(refreshedChannel, refreshedInfo); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery save schedule failed: %v", refreshedChannel.Id, refreshedChannel.Name, err))
		return false
	}

	logger.LogInfo(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s recovery scheduled at %s: %s", refreshedChannel.Id, refreshedChannel.Name, enableAtTime.Format(time.RFC3339), reason))
	scheduleCodexQuotaAutoEnableTimer(codexQuotaAutoEnableSchedule{
		channelID:   refreshedChannel.Id,
		channelName: refreshedChannel.Name,
		enableAt:    enableAt,
		disabledAt:  disabledAt,
	})
	return false
}

func scheduleCodexQuotaAutoEnableTimer(schedule codexQuotaAutoEnableSchedule) {
	if schedule.channelID <= 0 || schedule.enableAt <= 0 || schedule.disabledAt <= 0 {
		return
	}
	if current, ok := codexQuotaAutoEnableTimers.Load(schedule.channelID); ok {
		if current == schedule {
			return
		}
	}
	codexQuotaAutoEnableTimers.Store(schedule.channelID, schedule)

	delay := time.Until(time.Unix(schedule.enableAt, 0))
	if delay < 0 {
		delay = 0
	}
	gopool.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C

		current, ok := codexQuotaAutoEnableTimers.Load(schedule.channelID)
		if !ok || current != schedule {
			return
		}
		defer codexQuotaAutoEnableTimers.Delete(schedule.channelID)

		ch, err := model.GetChannelById(schedule.channelID, true)
		if err != nil || ch == nil {
			return
		}
		enableCodexChannelIfQuotaAutoEnableDue(context.Background(), ch)
	})
}

func codexQuotaScheduleFromInfo(channelID int, channelName string, info map[string]interface{}) codexQuotaAutoEnableSchedule {
	return codexQuotaAutoEnableSchedule{
		channelID:   channelID,
		channelName: channelName,
		enableAt:    int64FromMapValue(info[codexQuotaAutoEnableAtKey]),
		disabledAt:  int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]),
	}
}

func enableCodexChannelFromQuotaSchedule(ctx context.Context, ch *model.Channel, schedule codexQuotaAutoEnableSchedule) bool {
	if ch == nil {
		return false
	}
	info := ch.GetOtherInfo()
	if int64FromMapValue(info[codexQuotaAutoEnableAtKey]) != schedule.enableAt ||
		int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]) != schedule.disabledAt ||
		int64FromMapValue(info["status_time"]) != schedule.disabledAt {
		return false
	}
	delete(info, codexQuotaAutoEnableAtKey)
	delete(info, codexQuotaAutoEnableDisabledAtKey)
	delete(info, codexQuotaAutoEnableReasonKey)
	info["status_reason"] = ""
	info["status_time"] = common.GetTimestamp()

	otherInfoBytes, err := common.Marshal(info)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d marshal enabled other_info failed: %v", ch.Id, err))
		return false
	}

	result := model.DB.Model(&model.Channel{}).
		Where("id = ? AND type = ? AND status = ? AND other_info = ?", ch.Id, constant.ChannelTypeCodex, common.ChannelStatusAutoDisabled, ch.OtherInfo).
		Updates(map[string]interface{}{
			"status":     common.ChannelStatusEnabled,
			"other_info": string(otherInfoBytes),
		})
	if result.Error != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d enable failed: %v", ch.Id, result.Error))
		return false
	}
	if result.RowsAffected == 0 {
		return false
	}

	model.CacheUpdateChannelStatus(ch.Id, common.ChannelStatusEnabled)
	if err := model.UpdateAbilityStatus(ch.Id, true); err != nil {
		common.SysLog(fmt.Sprintf("failed to update ability status: channel_id=%d, error=%v", ch.Id, err))
	}
	logger.LogInfo(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d name=%s enabled at scheduled time", ch.Id, ch.Name))
	NotifyRootUser(formatNotifyType(ch.Id, common.ChannelStatusEnabled),
		fmt.Sprintf("通道「%s」（#%d）已被启用", ch.Name, ch.Id),
		fmt.Sprintf("通道「%s」（#%d）已被启用", ch.Name, ch.Id),
	)
	return true
}

func enableCodexChannelFromQuotaRecovery(ctx context.Context, ch *model.Channel, disabledAt int64) bool {
	if ch == nil || disabledAt <= 0 {
		return false
	}
	info := ch.GetOtherInfo()
	if int64FromMapValue(info["status_time"]) != disabledAt {
		return false
	}
	delete(info, codexQuotaAutoEnableAtKey)
	delete(info, codexQuotaAutoEnableDisabledAtKey)
	delete(info, codexQuotaAutoEnableReasonKey)
	info["status_reason"] = ""
	info["status_time"] = common.GetTimestamp()

	otherInfoBytes, err := common.Marshal(info)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d marshal recovery enabled other_info failed: %v", ch.Id, err))
		return false
	}

	result := model.DB.Model(&model.Channel{}).
		Where("id = ? AND type = ? AND status = ? AND other_info = ?", ch.Id, constant.ChannelTypeCodex, common.ChannelStatusAutoDisabled, ch.OtherInfo).
		Updates(map[string]interface{}{
			"status":     common.ChannelStatusEnabled,
			"other_info": string(otherInfoBytes),
		})
	if result.Error != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex quota auto-enable: channel_id=%d recovery enable failed: %v", ch.Id, result.Error))
		return false
	}
	if result.RowsAffected == 0 {
		return false
	}

	model.CacheUpdateChannelStatus(ch.Id, common.ChannelStatusEnabled)
	if err := model.UpdateAbilityStatus(ch.Id, true); err != nil {
		common.SysLog(fmt.Sprintf("failed to update ability status: channel_id=%d, error=%v", ch.Id, err))
	}
	NotifyRootUser(formatNotifyType(ch.Id, common.ChannelStatusEnabled),
		fmt.Sprintf("通道「%s」（#%d）已被启用", ch.Name, ch.Id),
		fmt.Sprintf("通道「%s」（#%d）已被启用", ch.Name, ch.Id),
	)
	return true
}

func saveCodexQuotaAutoEnableInfo(ch *model.Channel, info map[string]interface{}) error {
	if ch == nil {
		return fmt.Errorf("nil channel")
	}
	otherInfoBytes, err := common.Marshal(info)
	if err != nil {
		return err
	}
	result := model.DB.Model(&model.Channel{}).
		Where("id = ? AND type = ? AND status = ? AND other_info = ?", ch.Id, constant.ChannelTypeCodex, common.ChannelStatusAutoDisabled, ch.OtherInfo).
		Update("other_info", string(otherInfoBytes))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("channel status or other_info changed")
	}
	return nil
}

func isCodexUsageLimitDisabledInfo(info map[string]interface{}) bool {
	reason := strings.ToLower(strings.TrimSpace(fmt.Sprint(info["status_reason"])))
	return strings.Contains(reason, "status_code=429") && strings.Contains(reason, "usage limit")
}

func int64FromMapValue(value interface{}) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return parsed
	default:
		return 0
	}
}
