package billing

import "strings"

// Where a request's money comes from.
//
// Two ways a Fleet can answer a request, and they are not the same kind of
// thing:
//
//   - The fleet's own GPUs. The money was spent before the request arrived and
//     is split between tenants at the end of the month in proportion to what
//     they used. What one request cost is a share, not a price, and the figure
//     does not exist until the month closes. This is P8.
//   - A commercial API. The money leaves the account as the request runs, at a
//     rate the vendor published and Fleet did not choose. What one request cost
//     is knowable the moment it finishes.
//
// The distinction is carried on every ledger row rather than inferred at report
// time, for the same reason the tenant and project are: a row that only says
// "40 tokens of gpt-4o" cannot be allocated into a pool, and a row that only
// says "80 GPU-seconds" cannot be reconciled against a vendor invoice. Both
// need their answer recorded where the report cannot get it wrong.
//
// The two are deliberately not sumable. A tenant who spent $20 with a vendor
// and holds an $8 share of the pool owes $28 in the sense that both numbers
// are real; they do not owe a single $28 figure that means anything, because
// the $8 exists precisely because the pool contained other people's money and
// the $20 does not. Reports keep them apart for that reason, not for tidiness.
type Centre string

const (
	// CentrePool is money already spent on the fleet's own capacity.
	CentrePool Centre = "pool"
	// CentreDirect is money paid to somebody else per request.
	CentreDirect Centre = "direct"
)

// Provider names who served a request, or empty for the fleet's own engines.
//
// A name and not an identifier: nothing joins to it, nothing lists it, and an
// operator who buys from three vendors needs three strings rather than three
// rows in a table with a display name and a logo. The only thing a provider is
// used for is to pick a price book, and a price book is already keyed by model.
//
// Empty is the fleet's own capacity, which is why the centre is a function of
// this rather than a second column: a row that could say "direct" and name no
// provider would be a row nobody can price, and a row that could say "pool" and
// name one would be a row whose cost gets allocated twice.
type Provider = string

// CentreOf returns which invoice a provider's money reaches.
func CentreOf(provider Provider) Centre {
	if provider == "" {
		return CentrePool
	}
	return CentreDirect
}

// Valid reports whether a centre is one Fleet settles.
func (c Centre) Valid() bool { return c == CentrePool || c == CentreDirect }

// NormalizeProvider folds a provider name to the one form the ledger stores.
//
// Lower case and trimmed, so that "OpenAI", " openai" and "openai" are one
// price book rather than three, and a book declared under one spelling does not
// quietly stop applying to a route configured with another.
//
// Not validated against a list, because the list is the vendor's and changes
// faster than this code does. A misspelled provider is caught the moment it
// matters: the model has no book under it, and an unpriced model is a loud
// condition everywhere in this package rather than a silent one.
func NormalizeProvider(s Provider) Provider {
	return strings.ToLower(strings.TrimSpace(s))
}
