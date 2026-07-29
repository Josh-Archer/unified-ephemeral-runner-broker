package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// isWarmProvisionableBackend reports whether the broker may maintain warm
// allocations for the backend. Warm capacity targets cold-start external
// dispatch backends; fast or static adapters are excluded.
func isWarmProvisionableBackend(backendName model.BackendName) bool {
	switch backendName {
	case model.BackendCodeBuild,
		model.BackendLambda,
		model.BackendCloudRun,
		model.BackendAzureFunctions,
		model.BackendEC2,
		model.BackendGCE:
		return true
	default:
		// arc, azure-vm, desktop: expected to launch quickly or hold persistent runners.
		return false
	}
}

func resolveWarmTTL(cfg model.BackendConfig) time.Duration {
	if cfg.WarmTTL > 0 {
		return cfg.WarmTTL
	}
	return defaultWarmTTL
}

func normalizeWarmBounds(cfg model.BackendConfig) (int, int) {
	min := cfg.WarmMin
	max := cfg.WarmMax
	if min < 0 {
		min = 0
	}
	if max < 0 {
		max = 0
	}
	if max < min {
		max = min
	}
	return min, max
}

// effectiveWarmBounds returns the warmMin/warmMax that apply at now, honoring
// optional warmSchedule windows for cost-aware pre-warm.
func effectiveWarmBounds(cfg model.BackendConfig, now time.Time) (int, int, error) {
	baseMin, baseMax := normalizeWarmBounds(cfg)
	if cfg.WarmSchedule == nil || len(cfg.WarmSchedule.Windows) == 0 {
		return baseMin, baseMax, nil
	}

	loc, err := loadWarmLocation(cfg.WarmSchedule.Timezone)
	if err != nil {
		return 0, 0, err
	}
	local := now.In(loc)

	for _, window := range cfg.WarmSchedule.Windows {
		active, err := warmWindowActive(window, local)
		if err != nil {
			return 0, 0, err
		}
		if !active {
			continue
		}
		min, max := baseMin, baseMax
		if window.WarmMin != nil {
			min = *window.WarmMin
		}
		if window.WarmMax != nil {
			max = *window.WarmMax
		}
		if min < 0 {
			min = 0
		}
		if max < 0 {
			max = 0
		}
		if max < min {
			max = min
		}
		return min, max, nil
	}
	// Outside all configured windows: hold zero warm capacity.
	return 0, 0, nil
}

func loadWarmLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "UTC") || strings.EqualFold(name, "Local") {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("warmSchedule.timezone %q: %w", name, err)
	}
	return loc, nil
}

func warmWindowActive(window model.WarmWindowConfig, local time.Time) (bool, error) {
	if !warmWindowDayMatches(window.Days, local.Weekday()) {
		return false, nil
	}
	startMin, err := parseClockMinutes(window.Start)
	if err != nil {
		return false, fmt.Errorf("warmSchedule window start %q: %w", window.Start, err)
	}
	endMin, err := parseClockMinutes(window.End)
	if err != nil {
		return false, fmt.Errorf("warmSchedule window end %q: %w", window.End, err)
	}
	nowMin := local.Hour()*60 + local.Minute()
	if startMin == endMin {
		// Degenerate full-day window.
		return true, nil
	}
	if startMin < endMin {
		return nowMin >= startMin && nowMin < endMin, nil
	}
	// Crosses midnight: active if after start or before end.
	return nowMin >= startMin || nowMin < endMin, nil
}

func warmWindowDayMatches(days []string, weekday time.Weekday) bool {
	if len(days) == 0 {
		return true
	}
	want := weekdayToken(weekday)
	for _, day := range days {
		if normalizeWeekdayToken(day) == want {
			return true
		}
	}
	return false
}

func weekdayToken(day time.Weekday) string {
	switch day {
	case time.Monday:
		return "mon"
	case time.Tuesday:
		return "tue"
	case time.Wednesday:
		return "wed"
	case time.Thursday:
		return "thu"
	case time.Friday:
		return "fri"
	case time.Saturday:
		return "sat"
	default:
		return "sun"
	}
}

func normalizeWeekdayToken(day string) string {
	token := strings.ToLower(strings.TrimSpace(day))
	switch token {
	case "monday", "mon":
		return "mon"
	case "tuesday", "tue", "tues":
		return "tue"
	case "wednesday", "wed":
		return "wed"
	case "thursday", "thu", "thur", "thurs":
		return "thu"
	case "friday", "fri":
		return "fri"
	case "saturday", "sat":
		return "sat"
	case "sunday", "sun":
		return "sun"
	default:
		return token
	}
}

func parseClockMinutes(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty clock value")
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("expected HH:MM or HH:MM:SS")
	}
	var hour, minute int
	if _, err := fmt.Sscanf(parts[0], "%d", &hour); err != nil {
		return 0, fmt.Errorf("invalid hour")
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minute); err != nil {
		return 0, fmt.Errorf("invalid minute")
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("clock out of range")
	}
	if len(parts) == 3 {
		var second int
		if _, err := fmt.Sscanf(parts[2], "%d", &second); err != nil {
			return 0, fmt.Errorf("invalid second")
		}
		if second < 0 || second > 59 {
			return 0, fmt.Errorf("second out of range")
		}
	}
	return hour*60 + minute, nil
}

func normalizeWarmTarget(min, max, maxRunners int) int {
	if maxRunners <= 0 {
		return 0
	}
	if max > maxRunners {
		max = maxRunners
	}
	if min < 0 {
		min = 0
	}
	if min > max {
		min = max
	}
	return min
}

func isWarmExpired(status model.AllocationStatus, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return !status.ExpiresAt.IsZero() && !now.Before(status.ExpiresAt)
}

func filterWarmAllocations(statuses []model.AllocationStatus, poolName model.PoolName, backendName model.BackendName) []model.AllocationStatus {
	result := make([]model.AllocationStatus, 0)
	for _, status := range statuses {
		if status.Pool != poolName {
			continue
		}
		if status.SelectedBackend != backendName {
			continue
		}
		if status.State != model.StateWarm {
			continue
		}
		result = append(result, status)
	}
	return result
}

func filterFreshWarm(statuses []model.AllocationStatus, poolName model.PoolName, backendName model.BackendName, now time.Time, ttl time.Duration) []model.AllocationStatus {
	result := make([]model.AllocationStatus, 0)
	for _, status := range statuses {
		if status.Pool != poolName {
			continue
		}
		if status.SelectedBackend != backendName {
			continue
		}
		if status.State != model.StateWarm {
			continue
		}
		if !isWarmExpired(status, now, ttl) {
			result = append(result, status)
		}
	}
	return result
}
