package swaputil

import "testing"

func TestEvents_ResourceStateDoesNotCollideWithUpstream(t *testing.T) {
	resourceID := (ResourceStateChangeEvent{}).Type()
	for _, id := range []uint32{
		ProcessStateChangeEventID, ConfigFileChangedEventID, ActivityLogEventID,
		ModelPreloadedEventID, InFlightRequestsEventID, ProfileChangedEventID,
		ModelCapabilitiesChangedEventID, ProcessLoadingEventID,
	} {
		if resourceID == id {
			t.Fatalf("resource state event collides with upstream event %#x", id)
		}
	}
}
