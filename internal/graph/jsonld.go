package graph

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/qcoderx/sieve/internal/textnorm"
)

// StructuredFact is one whitelisted field lifted out of a page's JSON-LD.
type StructuredFact struct {
	// Type is the schema.org type it came from, e.g. "Organization".
	Type string `json:"type"`
	// Field is the whitelisted property name.
	Field string `json:"field"`
	// Value is the normalised, length-capped value.
	Value string `json:"value"`
}

// jsonLDValueCap bounds a single field. Structured data never renders, so
// nothing here was ever shown to a visitor and there is no legitimate reason
// for a field to be long.
const jsonLDValueCap = 400

// maxStructuredFacts bounds the whole section.
const maxStructuredFacts = 60

// jsonLDWhitelist is the complete set of properties read out of structured
// data, grouped by the questions they answer.
//
// A whitelist rather than a blocklist, and a short one. JSON-LD is the purest
// metadata channel on a page: it never renders, no visitor ever sees it, and a
// site can put a megabyte of arbitrary text there at no visible cost. Parsing
// it permissively would hand an attacker an unbounded, invisible channel
// straight into a model's context, in an artifact whose entire premise is token
// reduction.
//
// The cost of a whitelist is giving up fields nobody thought to include. That
// is the right trade: a missing founding date is an inconvenience, and an
// unbounded attacker-controlled string is not.
var jsonLDWhitelist = map[string]bool{
	// Identity
	"name": true, "legalName": true, "alternateName": true, "brand": true,
	"identifier": true, "vatID": true, "taxID": true, "duns": true,
	"description": true, "disambiguatingDescription": true, "slogan": true,
	// Provenance and dates
	"foundingDate": true, "datePublished": true, "dateModified": true,
	"dateCreated": true, "startDate": true, "endDate": true,
	// People and organisations
	"author": true, "creator": true, "publisher": true, "founder": true,
	"contactPoint": true, "telephone": true, "email": true,
	// Place
	"address": true, "addressLocality": true, "addressRegion": true,
	"addressCountry": true, "streetAddress": true, "postalCode": true,
	"areaServed": true, "location": true,
	// Commerce
	"price": true, "priceCurrency": true, "availability": true, "sku": true,
	"offers": true, "aggregateRating": true, "ratingValue": true,
	"reviewCount": true, "material": true, "color": true, "size": true,
	// Editorial
	"headline": true, "articleSection": true, "keywords": true,
	"inLanguage": true, "wordCount": true,
	// Structure
	"itemListElement": true, "position": true, "url": true,
	"openingHours": true, "openingHoursSpecification": true,
}

// QAPair is one question and its answer, lifted from FAQPage structured data.
//
// Questions and answers get their own shape rather than being flattened into
// StructuredFacts, because the pairing is the content. "What share of the
// revenue do you take?" and "It's agreed per partnership" are worth little
// apart and are exactly what an agent is sent to a page to find.
type QAPair struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// jsonLDAnswerCap bounds one answer.
//
// It is far larger than jsonLDValueCap because an answer is prose and is meant
// to be read, where a whitelisted scalar is a name or a date. It is still a cap:
// this is structured data, so no visitor ever saw it, and an uncapped field here
// is an invisible channel into a model's context.
const jsonLDAnswerCap = 1200

// maxQAPairs bounds the section.
const maxQAPairs = 30

// ParseFAQ lifts question-and-answer pairs out of FAQPage structured data.
//
// This is the one place where reading JSON-LD earns its risk. A page like
// pear.no publishes its entire FAQ as schema.org Question/acceptedAnswer and
// renders the same words into a scroll-revealed section that a static read
// cannot see and a browser has to travel to reach. The structured copy is
// exact, free, deterministic, and available in the first six hundred
// milliseconds of a run.
//
// The shape is narrow on purpose: only Question objects, only their name and
// the text of their acceptedAnswer, both normalised and capped, both marked
// with their provenance so a consumer can tell them from text a visitor saw.
// Nothing else in the FAQPage object is read.
func ParseFAQ(blobs []string) []QAPair {
	var out []QAPair
	seen := map[string]bool{}

	for _, blob := range blobs {
		var v any
		if err := json.Unmarshal([]byte(blob), &v); err != nil {
			continue
		}
		collectFAQ(v, &out, seen, 0)
		if len(out) >= maxQAPairs {
			break
		}
	}
	return out
}

func collectFAQ(v any, out *[]QAPair, seen map[string]bool, depth int) {
	if depth > 6 || len(*out) >= maxQAPairs {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			collectFAQ(e, out, seen, depth+1)
		}
	case map[string]any:
		if s, _ := t["@type"].(string); strings.EqualFold(s, "Question") {
			q := textnorm.CleanString(scalarString(t["name"]))
			if q == "" {
				q = textnorm.CleanString(scalarString(t["text"]))
			}
			a := answerText(t["acceptedAnswer"])
			if a == "" {
				a = answerText(t["suggestedAnswer"])
			}
			q, _ = textnorm.Truncate(q, jsonLDValueCap)
			a, _ = textnorm.Truncate(a, jsonLDAnswerCap)
			if q != "" && a != "" && !seen[q] {
				seen[q] = true
				*out = append(*out, QAPair{Question: q, Answer: a})
			}
			return
		}
		// Only descend through the containers a FAQPage actually uses. A
		// permissive walk here would turn every nested object on the page into
		// a candidate answer.
		for _, k := range []string{"mainEntity", "mainEntityOfPage", "itemListElement", "@graph", "hasPart"} {
			if sub, ok := t[k]; ok {
				collectFAQ(sub, out, seen, depth+1)
			}
		}
	}
}

// answerText reads the prose out of an Answer object, and nothing else.
func answerText(v any) string {
	switch t := v.(type) {
	case string:
		return textnorm.CleanString(t)
	case []any:
		for _, e := range t {
			if s := answerText(e); s != "" {
				return s
			}
		}
	case map[string]any:
		return textnorm.CleanString(scalarString(t["text"]))
	}
	return ""
}

// jsonLDContainers are properties whose value is a nested object worth
// descending into. Anything not listed is read as a scalar or ignored.
var jsonLDContainers = map[string]bool{
	"address": true, "offers": true, "aggregateRating": true,
	"contactPoint": true, "author": true, "publisher": true, "brand": true,
	"founder": true, "creator": true, "itemListElement": true,
	"openingHoursSpecification": true, "location": true,
}

// ParseJSONLD extracts whitelisted facts from a page's structured data.
//
// Malformed JSON is not an error worth reporting: a great many sites ship
// broken structured data and it says nothing about the page's content.
func ParseJSONLD(blobs []string) []StructuredFact {
	var out []StructuredFact
	seen := make(map[string]bool)

	for _, blob := range blobs {
		var v any
		if err := json.Unmarshal([]byte(blob), &v); err != nil {
			continue
		}
		collectLD(v, "", &out, seen, 0)
		if len(out) >= maxStructuredFacts {
			break
		}
	}
	// Stable order: the content hash must not depend on which script tag came
	// first when two carry the same facts.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > maxStructuredFacts {
		out = out[:maxStructuredFacts]
	}
	return out
}

func collectLD(v any, typeName string, out *[]StructuredFact, seen map[string]bool, depth int) {
	if depth > 6 || len(*out) >= maxStructuredFacts {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			collectLD(e, typeName, out, seen, depth+1)
		}
	case map[string]any:
		if s, ok := t["@type"].(string); ok && s != "" {
			typeName = s
		}
		// Deterministic iteration: Go randomises map order, and an artifact
		// hash that changes between runs of identical input is worse than no
		// hash at all.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			if k == "@type" || k == "@context" || k == "@id" {
				continue
			}
			if !jsonLDWhitelist[k] {
				continue
			}
			val := t[k]
			if jsonLDContainers[k] {
				if _, isScalar := val.(string); !isScalar {
					collectLD(val, typeName, out, seen, depth+1)
					continue
				}
			}
			s := scalarString(val)
			if s == "" {
				continue
			}
			s = textnorm.CleanString(s)
			s, _ = textnorm.Truncate(s, jsonLDValueCap)
			if s == "" {
				continue
			}
			key := typeName + "\x00" + k + "\x00" + s
			if seen[key] {
				continue
			}
			seen[key] = true
			*out = append(*out, StructuredFact{Type: typeName, Field: k, Value: s})
			if len(*out) >= maxStructuredFacts {
				return
			}
		}
	}
}

// scalarString renders a leaf value. Nested objects that reached here without
// being whitelisted containers are deliberately dropped rather than flattened:
// flattening is how an unbounded blob gets in through a field that looked safe.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return trimFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		var parts []string
		for _, e := range t {
			if s := scalarString(e); s != "" {
				parts = append(parts, s)
			}
			if len(parts) >= 12 {
				break
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func trimFloat(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}
