package gateway

import "os"

// DetailConfig points at the reporting replica.
//
// An empty URL means no store, and that is a complete gateway rather than a
// degraded one: Postgres is the ledger, so every report this store serves can be
// read from there. What is not acceptable is an operator who meant to configure a
// store, left the URL empty, and found out weeks later from a chart. So the URL
// is checked at startup and the rest is defaulted from it rather than the other
// way round.
type DetailConfig struct {
	URL      string
	Database string
	User     string
	Password string
}

// detailConfigFromEnv reads the replica's settings.
//
// The database and user have defaults while the URL does not, because those two
// are conventions an operator can be wrong about in a way that fails loudly --
// a missing table is an error the first query raises -- while a defaulted URL
// would point at nothing at all.
func detailConfigFromEnv() DetailConfig {
	return DetailConfig{
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
