package usagestore

import "time"

// The table stores every timestamp as fractional epoch seconds, which is the
// on-disk contract. These convert at the boundary so nothing above it has to
// think in floats.

// fromEpoch reads a stored timestamp.
//
// An ABSENT value becomes the zero [time.Time], which means "unknown"
// everywhere above. A stored ZERO becomes the Unix epoch, which is a real
// instant in the distant past — and the distinction matters: a released lease is
// written as an explicit zero precisely so it reads as "expired long ago"
// rather than "never fenced", which would send liveClaim down its legacy path
// and keep a just-released row looking claimed.
func fromEpoch(seconds *float64) time.Time {
	if seconds == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*seconds * 1000)).UTC()
}

// toEpoch writes a timestamp, storing nothing for the zero value.
func toEpoch(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	seconds := float64(t.UnixMilli()) / 1000
	return &seconds
}

// epochZero is the explicit zero a released lease is written as.
func epochZero() *float64 {
	zero := 0.0
	return &zero
}

// durationFromSeconds reads a stored interval, returning zero when absent.
func durationFromSeconds(seconds *float64) time.Duration {
	if seconds == nil {
		return 0
	}
	return time.Duration(*seconds * float64(time.Second))
}

// secondsFromDuration writes an interval, storing nothing for zero.
func secondsFromDuration(d time.Duration) *float64 {
	if d == 0 {
		return nil
	}
	seconds := d.Seconds()
	return &seconds
}
