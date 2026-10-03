package cost

import "github.com/zlogic-labs/fleet/core/pkg/billing"

// Turning GPU-seconds into money, and parts of a pool into shares.

func rateIndex(rates []Rate) map[string]Rate {
	byCluster := make(map[string]Rate, len(rates))
	for _, r := range rates {
		byCluster[r.Cluster] = r
	}
	return byCluster
}

// priceSeconds converts GPU-seconds at a per-GPU-hour rate.
//
// The rate is already in micro-units, so the divisor is just an hour. Scaling by
// MicroPerUnit here as well prices a month at a millionth of what it cost, which
// is a plausible-looking number and wildly wrong.
func priceSeconds(seconds, rateMicro int64) billing.Amount {
	const gpuSecondsPerHour = 3600
	num := seconds * rateMicro
	return billing.Amount((num + gpuSecondsPerHour/2) / gpuSecondsPerHour)
}

// proportion is part of pool, by part of whole, clamped to pool.
func proportion(pool billing.Amount, part, whole int64) billing.Amount {
	if pool <= 0 || whole <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return pool
	}
	return billing.Amount(int64(pool) * part / whole)
}

// percent is part/whole as a whole-number percentage, rounded half up.
//
// Half up rather than truncating because IdlePct is read as "78% idle", and
// truncation calls a fleet that is 78.6% idle 78%, which flatters it.
func percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	if part < 0 {
		return -percent(-part, whole)
	}
	const hundred = 100
	return int((part*hundred*2 + whole) / (whole * 2))
}
