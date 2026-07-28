package llm

import "strings"

// wantsJSONArray reports whether a prompt asks for a top-level JSON array.
//
// Such a prompt must NOT be sent with `response_format: json_object`: that mode
// constrains the model to a top-level object, the two requirements cannot both
// hold, and models settle it by returning `{}` — syntactically valid, entirely
// empty. The caller then sees a parse failure rather than an API error, which
// is how this hid.
//
// Both of mnemos's array prompts are matched by this: the extraction prompt
// ("Output format — a JSON array of objects") and the durability classifier
// ("Reply with ONLY a JSON array of objects").
//
// A substring test is a blunt instrument, and it is deliberately the blunt one
// that fails SAFE: a false positive merely declines to constrain a model that
// would probably have complied anyway, while a false negative produces `{}` and
// a silent fallback. Prefer under-constraining.
func wantsJSONArray(prompt string) bool {
	return strings.Contains(strings.ToLower(prompt), "json array")
}
