package tokenizer

// Heuristic token costs, tuned to bias *high* on purpose. An over-estimate
// rejects a request that would have fit; an under-estimate lets real spend
// escape the limiter, which is the failure mode P5 exists to prevent. The
// engine's usage field corrects the ledger afterwards, so a temporary
// over-reserve is refunded at settlement.
const (
	asciiTokensPerRune = 0.25
	wideTokensPerRune  = 1.5
)

// heuristic is the fallback tokenizer. It is stateless and always safe.
type heuristic struct{ bias float64 }

func (h heuristic) Count(text string) int {
	if text == "" {
		return 0
	}
	var ascii, wide int
	for _, r := range text {
		if r < 0x80 {
			ascii++
		} else {
			wide++
		}
	}
	n := float64(ascii)*asciiTokensPerRune + float64(wide)*wideTokensPerRune
	if scaled := int(n*h.bias + 0.5); scaled > 0 {
		return scaled
	}
	// Never return 0 for non-empty text: a zero-token request would slip past
	// every quota that is expressed in tokens.
	return 1
}

func (h heuristic) Kind() Kind       { return KindHeuristic }
func (h heuristic) Encoding() string { return "" }

// NewHeuristic returns the character-class estimator, optionally biased by a
// multiplier where 1.0 is the calibrated default.
func NewHeuristic(bias float64) Tokenizer {
	if bias <= 0 {
		bias = 1
	}
	return heuristic{bias: bias}
}
