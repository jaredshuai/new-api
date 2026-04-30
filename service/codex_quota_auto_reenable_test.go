package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCodexQuotaAutoEnableTest(t *testing.T) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	oldDB := model.DB
	oldLogDB := model.LOG_DB
	oldAutoEnable := common.AutomaticEnableChannelEnabled
	oldMemoryCache := common.MemoryCacheEnabled
	oldRedis := common.RedisEnabled

	model.DB = db
	model.LOG_DB = db
	common.AutomaticEnableChannelEnabled = true
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	resetCodexQuotaAutoEnableTimersForTest()

	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}))
	require.NoError(t, db.Create(&model.User{
		Id:       1,
		Username: "root",
		Password: "password123",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
	}).Error)

	t.Cleanup(func() {
		resetCodexQuotaAutoEnableTimersForTest()
		model.DB = oldDB
		model.LOG_DB = oldLogDB
		common.AutomaticEnableChannelEnabled = oldAutoEnable
		common.MemoryCacheEnabled = oldMemoryCache
		common.RedisEnabled = oldRedis
		_ = sqlDB.Close()
	})
}

func resetCodexQuotaAutoEnableTimersForTest() {
	codexQuotaAutoEnableTimers.Range(func(key, _ interface{}) bool {
		codexQuotaAutoEnableTimers.Delete(key)
		return true
	})
}

func codexQuotaOtherInfoForTest(t *testing.T, disabledAt int64, enableAt int64) string {
	t.Helper()
	info := map[string]interface{}{
		"status_reason":                   "quota exhausted",
		"status_time":                     disabledAt,
		codexQuotaAutoEnableAtKey:         enableAt,
		codexQuotaAutoEnableDisabledAtKey: disabledAt,
		codexQuotaAutoEnableReasonKey:     "codex 5-hour quota window reset",
	}
	body, err := common.Marshal(info)
	require.NoError(t, err)
	return string(body)
}

func codexQuotaDisabledOtherInfoForTest(t *testing.T, disabledAt int64) string {
	t.Helper()
	info := map[string]interface{}{
		"status_reason": "status_code=429, The usage limit has been reached",
		"status_time":   disabledAt,
	}
	body, err := common.Marshal(info)
	require.NoError(t, err)
	return string(body)
}

func codexOAuthKeyForTest(t *testing.T) string {
	t.Helper()
	body, err := common.Marshal(CodexOAuthKey{
		AccessToken: "access-token",
		AccountID:   "account-1",
		Type:        "codex",
		Expired:     time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	return string(body)
}

func createCodexQuotaChannelForTest(t *testing.T, channel *model.Channel) {
	t.Helper()
	if channel.Name == "" {
		channel.Name = "codex-test"
	}
	if channel.Type == 0 {
		channel.Type = constant.ChannelTypeCodex
	}
	if channel.Status == 0 {
		channel.Status = common.ChannelStatusAutoDisabled
	}
	require.NoError(t, model.DB.Create(channel).Error)
}

func TestScheduleCodexQuotaAutoEnableAfterDisableSyncStoresGraceSchedule(t *testing.T) {
	setupCodexQuotaAutoEnableTest(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("unexpected authorization header: %s", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "account-1" {
			t.Errorf("unexpected account header: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rate_limit":{
				"allowed":false,
				"limit_reached":true,
				"primary_window":{"used_percent":100,"reset_after_seconds":300,"limit_window_seconds":18000},
				"secondary_window":{"used_percent":40,"reset_after_seconds":3600,"limit_window_seconds":604800}
			}
		}`))
	}))
	defer server.Close()

	channelID := 101
	disabledAt := common.GetTimestamp() - 30
	baseURL := server.URL
	createCodexQuotaChannelForTest(t, &model.Channel{
		Id:        channelID,
		Key:       codexOAuthKeyForTest(t),
		BaseURL:   &baseURL,
		OtherInfo: codexQuotaOtherInfoForTest(t, disabledAt, 0),
	})

	before := time.Now().Unix()
	scheduleCodexQuotaAutoEnableAfterDisableSync(context.Background(), channelID, "codex-test")
	after := time.Now().Unix()

	reloaded, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	info := reloaded.GetOtherInfo()
	enableAt := int64FromMapValue(info[codexQuotaAutoEnableAtKey])
	graceSeconds := int64(codexQuotaAutoEnableGracePeriod / time.Second)
	require.GreaterOrEqual(t, enableAt, before+300+graceSeconds)
	require.LessOrEqual(t, enableAt, after+301+graceSeconds)
	require.Equal(t, disabledAt, int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]))
	require.Equal(t, int32(1), requests.Load())
}

func TestEnableCodexChannelIfQuotaAutoEnableDueDoesNotEnableManualDisabledRace(t *testing.T) {
	setupCodexQuotaAutoEnableTest(t)

	channelID := 102
	disabledAt := common.GetTimestamp() - 60
	enableAt := common.GetTimestamp() - 1
	createCodexQuotaChannelForTest(t, &model.Channel{
		Id:        channelID,
		Key:       "not-json",
		OtherInfo: codexQuotaOtherInfoForTest(t, disabledAt, enableAt),
	})
	staleSnapshot, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channelID).Update("status", common.ChannelStatusManuallyDisabled).Error)

	enabled := enableCodexChannelIfQuotaAutoEnableDue(context.Background(), staleSnapshot)

	require.False(t, enabled)
	reloaded, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusManuallyDisabled, reloaded.Status)
}

func TestRunCodexCredentialAutoRefreshOnceEnablesDueQuotaScheduleBeforeKeyParsing(t *testing.T) {
	setupCodexQuotaAutoEnableTest(t)

	channelID := 103
	disabledAt := common.GetTimestamp() - 60
	enableAt := common.GetTimestamp() - 1
	createCodexQuotaChannelForTest(t, &model.Channel{
		Id:        channelID,
		Key:       "not-json",
		OtherInfo: codexQuotaOtherInfoForTest(t, disabledAt, enableAt),
	})
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     "gpt-5",
		ChannelId: channelID,
		Enabled:   false,
	}).Error)

	runCodexCredentialAutoRefreshOnce()

	reloaded, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	info := reloaded.GetOtherInfo()
	require.Zero(t, int64FromMapValue(info[codexQuotaAutoEnableAtKey]))
	require.Zero(t, int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]))

	var ability model.Ability
	require.NoError(t, model.DB.Where("channel_id = ?", channelID).First(&ability).Error)
	require.True(t, ability.Enabled)
}

func TestRunCodexCredentialAutoRefreshOnceRecoversQuotaDisabledChannelWithoutSchedule(t *testing.T) {
	setupCodexQuotaAutoEnableTest(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rate_limit":{
				"allowed":true,
				"limit_reached":false,
				"primary_window":{"used_percent":0,"reset_after_seconds":18000,"limit_window_seconds":18000},
				"secondary_window":{"used_percent":62,"reset_after_seconds":498206,"limit_window_seconds":604800}
			}
		}`))
	}))
	defer server.Close()

	channelID := 104
	disabledAt := common.GetTimestamp() - 3600
	baseURL := server.URL
	createCodexQuotaChannelForTest(t, &model.Channel{
		Id:        channelID,
		Key:       codexOAuthKeyForTest(t),
		BaseURL:   &baseURL,
		OtherInfo: codexQuotaDisabledOtherInfoForTest(t, disabledAt),
	})
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     "gpt-5",
		ChannelId: channelID,
		Enabled:   false,
	}).Error)

	runCodexCredentialAutoRefreshOnce()

	reloaded, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	info := reloaded.GetOtherInfo()
	require.Empty(t, info["status_reason"])
	require.Zero(t, int64FromMapValue(info[codexQuotaAutoEnableAtKey]))
	require.Zero(t, int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]))
	require.Equal(t, int32(1), requests.Load())

	var ability model.Ability
	require.NoError(t, model.DB.Where("channel_id = ?", channelID).First(&ability).Error)
	require.True(t, ability.Enabled)
}

func TestRunCodexCredentialAutoRefreshOnceSchedulesQuotaDisabledChannelWithoutSchedule(t *testing.T) {
	setupCodexQuotaAutoEnableTest(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rate_limit":{
				"allowed":false,
				"limit_reached":true,
				"primary_window":{"used_percent":100,"reset_after_seconds":300,"limit_window_seconds":18000},
				"secondary_window":{"used_percent":62,"reset_after_seconds":498206,"limit_window_seconds":604800}
			}
		}`))
	}))
	defer server.Close()

	channelID := 105
	disabledAt := common.GetTimestamp() - 3600
	baseURL := server.URL
	createCodexQuotaChannelForTest(t, &model.Channel{
		Id:        channelID,
		Key:       codexOAuthKeyForTest(t),
		BaseURL:   &baseURL,
		OtherInfo: codexQuotaDisabledOtherInfoForTest(t, disabledAt),
	})

	before := time.Now().Unix()
	runCodexCredentialAutoRefreshOnce()
	after := time.Now().Unix()

	reloaded, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	info := reloaded.GetOtherInfo()
	enableAt := int64FromMapValue(info[codexQuotaAutoEnableAtKey])
	graceSeconds := int64(codexQuotaAutoEnableGracePeriod / time.Second)
	require.GreaterOrEqual(t, enableAt, before+300+graceSeconds)
	require.LessOrEqual(t, enableAt, after+301+graceSeconds)
	require.Equal(t, disabledAt, int64FromMapValue(info[codexQuotaAutoEnableDisabledAtKey]))
	require.Equal(t, int32(1), requests.Load())
}
