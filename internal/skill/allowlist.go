package skill

// Allowlist limits which skills one agent can see and load. It exists because
// the answer has three states, not two, and threading a *[]string through every
// signature would spread that subtlety across the codebase instead of holding it
// in one place:
//
//   - unset (nil)      ⇒ every enabled skill. What an agent defined before this
//     field existed gets, so adding the field changes nothing.
//   - a list of names  ⇒ exactly those skills.
//   - an empty list    ⇒ none. This is the coordinator's case: an agent whose
//     job is to decide who handles a request should not also be holding the
//     tools to handle it itself.
//
// The zero Allowlist is unrestricted, so a caller that has no opinion can pass
// Allowlist{}.
type Allowlist struct {
	names map[string]bool // nil ⇒ unrestricted
}

// NewAllowlist builds an Allowlist from a config field. A nil pointer means the
// field was never set (unrestricted); a pointer to an empty slice means it was
// set to nothing (no skills).
func NewAllowlist(names *[]string) Allowlist {
	if names == nil {
		return Allowlist{}
	}
	m := make(map[string]bool, len(*names))
	for _, n := range *names {
		if n != "" {
			m[n] = true
		}
	}
	return Allowlist{names: m}
}

// Permits reports whether this agent may see and load the named skill.
func (a Allowlist) Permits(name string) bool {
	return a.names == nil || a.names[name]
}

// Unrestricted reports whether the allowlist imposes no limit at all.
func (a Allowlist) Unrestricted() bool { return a.names == nil }

// Denies everything reports whether the agent was given no skills on purpose.
// Callers use it to skip registering the skill tools entirely rather than
// register tools that can only ever answer "not found".
func (a Allowlist) DeniesAll() bool { return a.names != nil && len(a.names) == 0 }
