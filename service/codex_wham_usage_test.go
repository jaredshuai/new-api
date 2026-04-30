package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexWhamUsageAutoEnableTimeUsesExhaustedWindowReset(t *testing.T) {
	now := time.Unix(1000, 0)
	body := []byte(`{
		"rate_limit":{
			"allowed":false,
			"limit_reached":true,
			"primary_window":{"used_percent":100,"reset_after_seconds":300,"limit_window_seconds":18000},
			"secondary_window":{"used_percent":40,"reset_after_seconds":3600,"limit_window_seconds":604800}
		}
	}`)

	enableAt, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, now.Add(300*time.Second), enableAt)
	require.Contains(t, reason, "5-hour")
}

func TestCodexWhamUsageAutoEnableTimeInvalidJSON(t *testing.T) {
	_, _, ok, err := CodexWhamUsageAutoEnableTime([]byte(`not-json`), time.Unix(1000, 0))

	require.Error(t, err)
	require.False(t, ok)
}

func TestCodexWhamUsageAutoEnableTimeWaitsForLaterResetWhenBothWindowsExhausted(t *testing.T) {
	now := time.Unix(1000, 0)
	body := []byte(`{
		"rate_limit":{
			"allowed":false,
			"limit_reached":true,
			"primary_window":{"used_percent":100,"reset_after_seconds":300,"limit_window_seconds":18000},
			"secondary_window":{"used_percent":100,"reset_after_seconds":3600,"limit_window_seconds":604800}
		}
	}`)

	enableAt, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, now.Add(3600*time.Second), enableAt)
	require.Contains(t, reason, "5-hour and weekly")
}

func TestCodexWhamUsageAutoEnableTimeRequiresExhaustedWindowReset(t *testing.T) {
	now := time.Unix(1000, 0)
	body := []byte(`{
		"rate_limit":{
			"allowed":false,
			"limit_reached":true,
			"primary_window":{"used_percent":100,"limit_window_seconds":18000},
			"secondary_window":{"used_percent":40,"reset_after_seconds":3600,"limit_window_seconds":604800}
		}
	}`)

	_, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)

	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, reason, "reset time")
}

func TestCodexWhamUsageAutoEnableTimeRefusesPartialResetWhenBothWindowsExhausted(t *testing.T) {
	now := time.Unix(1000, 0)
	body := []byte(`{
		"rate_limit":{
			"allowed":false,
			"limit_reached":true,
			"primary_window":{"used_percent":100,"reset_after_seconds":300,"limit_window_seconds":18000},
			"secondary_window":{"used_percent":100,"limit_window_seconds":604800}
		}
	}`)

	_, reason, ok, err := CodexWhamUsageAutoEnableTime(body, now)

	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, reason, "weekly")
	require.Contains(t, reason, "missing reset time")
}
