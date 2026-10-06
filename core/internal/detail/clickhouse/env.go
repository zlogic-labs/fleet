package clickhouse

import "os"

// FromEnv reads the replica's settings.
//
// It lives beside the store rather than in each binary because two processes now
// read these variables — the gateway mirrors into the replica, the control plane
// compares against it — and a name that drifted between them would leave one of
// the two pointing at nothing while the other worked.
//
// The database and the user have defaults while the URL does not, because those
// two are conventions an operator can be wrong about in a way that fails loudly
// — a missing table is an error the first query raises — while a defaulted URL
// would point at nothing at all.
func FromEnv() Config {
	return Config{
		URL:      os.Getenv("FLEET_CLICKHOUSE_URL"),
		Database: envDefault("FLEET_CLICKHOUSE_DATABASE", "fleet_detail"),
		User:     envDefault("FLEET_CLICKHOUSE_USER", "default"),
		Password: os.Getenv("FLEET_CLICKHOUSE_PASSWORD"),
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
