package prom

import "testing"

// The parser reads an engine's /metrics and, since the exporter was added, the
// gateway's own. Both are written by something Fleet does not control the
// escaping of, and a silently corrupted label value produces a wrong routing
// decision rather than an error.

func TestALabelValueIsUnescapedNotTrimmed(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`m{a="plain"} 1`, "plain"},
		{`m{a="with space"} 1`, "with space"},
		{`m{a="line\nbreak"} 1`, "line\nbreak"},
		// The case that was wrong: the value ends in an escaped quote, so the
		// raw text ends `\""`. Trimming quotes as a run removes both and
		// leaves the backslash dangling.
		{`m{a="say \"hi\""} 1`, `say "hi"`},
		{`m{a="",b="x"} 1`, ""},
		{`m{a="a,b",b="y"} 1`, "a,b"},
	}
	for _, c := range cases {
		samples := Parse([]byte(c.line))
		if len(samples) != 1 {
			t.Errorf("%s produced %d samples, want 1", c.line, len(samples))
			continue
		}
		if got, _ := samples[0].Label("a"); got != c.want {
			t.Errorf("%s gave a=%q, want %q", c.line, got, c.want)
		}
	}
}

func TestALabelRenderedAsNoneIsAbsentNotZero(t *testing.T) {
	// vLLM stringifies every unset CacheConfig field as the literal "None".
	// Reading that as 0 is how an engine that published no capacity at all
	// looks like one with an idle, empty cache.
	samples := Parse([]byte(`vllm:cache_config_info{kv_cache_size_tokens="None",block_size="16"} 1`))
	s := samples[0]
	if _, ok := s.Label("kv_cache_size_tokens"); ok != true {
		t.Fatal("the label is missing entirely")
	}
	if _, ok := IntLabel(s, "kv_cache_size_tokens"); ok {
		t.Error(`IntLabel read "None" as a number`)
	}
	if n, ok := IntLabel(s, "block_size"); !ok || n != 16 {
		t.Errorf("block_size = %d (%v), want 16", n, ok)
	}
}
