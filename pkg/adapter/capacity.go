package adapter

// FreeSlots returns non-negative free runner slots from a capacity snapshot.
// The formula is max(0, MaxRunners-ActiveRunners-PendingRunners-WarmRunners)
// and matches the broker's provider-capacity accounting.
func FreeSlots(status CapacityStatus) int {
	used := status.ActiveRunners + status.PendingRunners + status.WarmRunners
	free := status.MaxRunners - used
	if free < 0 {
		return 0
	}
	return free
}
