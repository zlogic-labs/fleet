package clickhouse

// detailBreakdown writes a token sub-count, or NULL when the engine reported
// none.
//
// The same rule the ledger applies, duplicated rather than shared: the two
// stores are separate modules on purpose -- one is the book, the other is a
// mirror that may be dropped and rebuilt -- and a mirror that imports the
// book's write helpers is a mirror that will change whenever the book does.
//
// Storing 0 instead would make the mirror claim "no cached tokens" on rows
// where the engine simply omitted the field, which is the difference between a
// measurement and a guess.
func detailBreakdown(n int, present bool) *int64 {
	if !present {
		return nil
	}
	v := int64(n)
	return &v
}
