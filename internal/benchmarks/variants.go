package benchmarks

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

type Target struct {
	ID, Provider, Name, DefaultEffort string
	ReportedNames, Efforts            []string
}

type Binding struct {
	EntryID  string            `json:"entry_id"`
	Efforts  map[string]string `json:"efforts,omitempty"`
	Identity string            `json:"identity,omitempty"`
}

type Variant struct {
	Result
	ID     string `json:"id"`
	Effort string `json:"effort,omitempty"`
}

type Evaluation struct {
	*Result
	Variants      []Variant `json:"variants,omitempty"`
	DefaultEffort string    `json:"default_effort,omitempty"`
}

type Family struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Creator  string    `json:"creator,omitempty"`
	Variants []Variant `json:"variants"`
}

type Catalog struct {
	families []*Family
	byID     map[string]*Family
	byKey    map[string][]*Family
}

var variantName = regexp.MustCompile(`(?i)\s*\((minimal|low|medium|high|xhigh|max|non[- ]reasoning|thinking|reasoning)\)$`)
var revisionSuffix = regexp.MustCompile(`^(?:20[0-9]{2}-[0-9]{2}-[0-9]{2}|[0-9]{4}|[0-9]{8})$`)

func exactKey(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return strings.ContainsRune(" -_.()\t", r)
	}), "-")
}

func NormalizeEffort(s string) string {
	switch key := exactKey(s); key {
	case "none", "off", "non-reasoning":
		return "off"
	default:
		return key
	}
}

func newCatalog(rows []aaModel) *Catalog {
	c := &Catalog{byID: map[string]*Family{}, byKey: map[string][]*Family{}}
	groups := map[string]*Family{}
	for _, row := range rows {
		if row.ID == "" || row.Name == "" {
			continue
		}
		name, effort := row.Name, ""
		if parts := variantName.FindStringSubmatch(row.Name); parts != nil {
			name = strings.TrimSpace(strings.TrimSuffix(row.Name, parts[0]))
			effort = NormalizeEffort(parts[1])
		}
		key := exactKey(row.Creator.Name) + "/" + exactKey(name)
		family := groups[key]
		if family == nil {
			family = &Family{ID: row.ID, Name: name, Creator: row.Creator.Name}
			groups[key] = family
			c.families = append(c.families, family)
		}
		family.Variants = append(family.Variants, Variant{Result: row.result(), ID: row.ID, Effort: effort})
		c.byID[row.ID] = family
		slug := exactKey(row.Slug)
		familySlug := slug
		if effort != "" {
			suffix := effort
			if effort == "off" {
				suffix = "non-reasoning"
			}
			familySlug = strings.TrimSuffix(slug, "-"+suffix)
		}
		for _, k := range []string{exactKey(row.ID), exactKey(name), exactKey(row.Name), slug, familySlug} {
			if k != "" && !slices.Contains(c.byKey[k], family) {
				c.byKey[k] = append(c.byKey[k], family)
			}
		}
	}
	return c
}

func (s *Store) Catalog() *Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalog
}

func (t Target) Identity() string {
	names := t.names()
	slices.Sort(names)
	return strings.Join(names, "\n")
}

func (t Target) names() []string {
	names := make([]string, 0, len(t.ReportedNames)+1)
	for _, name := range append(slices.Clone(t.ReportedNames), t.Name) {
		key := exactKey(name)
		if key != "" && !slices.Contains(names, key) {
			names = append(names, key)
		}
	}
	return names
}

func (c *Catalog) automatic(t Target) (*Family, bool) {
	id := exactKey(seed(t.Provider, t.ID))
	names := t.names()
	// Prefer a reported dated revision to the channel's stable alias when the
	// spelling preserves the entire base ID and adds only a revision.
	revision := ""
	for _, name := range names {
		if tail, ok := strings.CutPrefix(name, id+"-"); ok && revisionSuffix.MatchString(tail) {
			if revision != "" && revision != name {
				return nil, true
			}
			revision = name
		}
	}
	if revision != "" && (len(names) == 1 || len(names) == 2 && slices.Contains(names, id)) {
		// An alias and a revision reported by different accounts are not enough
		// to establish a common deployment.
		if len(names) > 1 && len(t.ReportedNames) > 1 {
			return nil, true
		}
		id = revision
		names = []string{revision}
	}
	var found *Family
	for _, key := range names {
		options := c.byKey[key]
		if len(options) > 1 {
			return nil, false
		}
		if len(options) == 1 && found != nil && found != options[0] {
			return nil, true
		}
		if len(options) == 1 {
			found = options[0]
		}
	}
	options := c.byKey[id]
	if len(options) > 1 || len(options) == 1 && found != nil && found != options[0] {
		return nil, false
	}
	if len(options) == 1 {
		return options[0], false
	}
	return found, false
}

func (c *Catalog) BindingState(t Target, binding Binding) string {
	_, conflict := c.automatic(t)
	if conflict {
		return "conflict"
	}
	if binding.EntryID != "" && (binding.Identity != t.Identity() || c.byID[binding.EntryID] == nil || c.validateEfforts(t, binding) != nil) {
		return "review"
	}
	return ""
}

func (c *Catalog) ValidateBinding(t Target, b Binding) error {
	if _, conflict := c.automatic(t); conflict {
		return errors.New("渠道目录中的模型版本存在冲突，无法绑定统一评测")
	}
	family := c.byID[b.EntryID]
	if family == nil {
		return errors.New("所选 AA 评测条目已不存在，请刷新后重新选择")
	}
	id := exactKey(seed(t.Provider, t.ID))
	for _, name := range t.names() {
		if tail, ok := strings.CutPrefix(name, id+"-"); ok && revisionSuffix.MatchString(tail) && !slices.Contains(c.byKey[name], family) {
			return errors.New("所选评测与渠道明确报告的版本不一致")
		}
	}
	return c.validateEfforts(t, b)
}

func (c *Catalog) validateEfforts(t Target, b Binding) error {
	family := c.byID[b.EntryID]
	used := map[string]bool{}
	for effort, id := range b.Efforts {
		if !slices.Contains(t.Efforts, effort) || c.byID[id] != family || used[id] {
			return errors.New("所选档位或评测条目不属于当前模型版本，或存在重复绑定")
		}
		for _, v := range family.Variants {
			if v.ID == id && v.Effort != "" && v.Effort != "thinking" && v.Effort != "reasoning" && v.Effort != effort {
				return errors.New("所选评测的明确档位与渠道档位不一致")
			}
			if v.ID == id && effort == "off" && v.Effort != "off" {
				return errors.New("关闭思考只能绑定明确标为关闭思考的评测")
			}
		}
		used[id] = true
	}
	return nil
}

func (c *Catalog) Resolve(t Target, binding Binding) *Evaluation {
	if c == nil || c.BindingState(t, binding) != "" {
		return nil
	}
	family := c.byID[binding.EntryID]
	if family == nil {
		family, _ = c.automatic(t)
	}
	if family == nil {
		return nil
	}
	var evaluation Evaluation
	assigned := map[string]bool{}
	for _, id := range binding.Efforts {
		assigned[id] = true
	}
	for _, effort := range t.Efforts {
		var found *Variant
		override := binding.Efforts[effort]
		for _, v := range family.Variants {
			if override == v.ID || override == "" && !assigned[v.ID] && effort == v.Effort {
				if found != nil {
					found = nil
					break
				}
				found = &v
			}
		}
		if found != nil {
			found.Effort = effort
			evaluation.Variants = append(evaluation.Variants, *found)
			if effort == t.DefaultEffort {
				evaluation.DefaultEffort = effort
				if found.hasScore() {
					evaluation.Result = &found.Result
				}
			}
		}
	}
	if len(evaluation.Variants) > 0 {
		return &evaluation
	}
	for _, v := range family.Variants {
		if v.Effort == "" && v.hasData() {
			if evaluation.Result != nil {
				return nil
			}
			evaluation.Result = &v.Result
		}
	}
	if evaluation.Result != nil {
		return &evaluation
	}
	// A catalog can identify the release without declaring a reasoning mode.
	// Show scored AA variants as references, with no channel effort assigned.
	if len(t.Efforts) == 0 {
		for _, v := range family.Variants {
			if v.hasData() {
				v.Effort = ""
				evaluation.Variants = append(evaluation.Variants, v)
			}
		}
		if len(evaluation.Variants) > 0 {
			return &evaluation
		}
	}
	return nil
}

func (c *Catalog) Candidates(t Target, binding Binding, query string) []Family {
	out := []Family{}
	if c == nil {
		return out
	}
	current := c.byID[binding.EntryID]
	if current != nil {
		out = append(out, *current)
	}
	if q := exactKey(query); q != "" {
		for _, f := range c.families {
			if f != current && (strings.Contains(exactKey(f.Creator+" "+f.Name), q) ||
				slices.ContainsFunc(f.Variants, func(v Variant) bool { return strings.Contains(exactKey(v.Name), q) })) {
				out = append(out, *f)
				if len(out) == 25 {
					break
				}
			}
		}
		return out
	}
	if f, _ := c.automatic(t); f != nil && f != current {
		out = append(out, *f)
	}
	prefixes := append(t.names(), exactKey(seed(t.Provider, t.ID)))
	for _, f := range c.families {
		if slices.ContainsFunc(out, func(option Family) bool { return option.ID == f.ID }) {
			continue
		}
		name := exactKey(f.Name)
		if slices.ContainsFunc(prefixes, func(key string) bool {
			return len(key) >= 4 && (name == key || strings.HasPrefix(name, key+"-"))
		}) {
			out = append(out, *f)
			if len(out) == 25 {
				break
			}
		}
	}
	return out
}

func (r Result) hasScore() bool {
	return r.Intelligence != nil || r.Coding != nil || r.Math != nil
}

func (r Result) hasData() bool {
	return r.hasScore() || r.OutputTokensPerSecond != nil || r.FirstAnswerSeconds != nil ||
		r.InputPerMillion != nil || r.OutputPerMillion != nil
}
