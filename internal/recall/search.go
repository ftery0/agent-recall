package recall

import (
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Search scoring, after deja-vu: BM25 over Korean bigrams and identifier pieces, a request a unit.
const (
	bm25K1      = 1.2
	bm25B       = 0.75
	humanWeight = 1.3 // the person's words count more than the agent's answer
)

// Match tiers, best first. A document holding none of the query's words is not a result: bigrams
// only rank, the stripped word itself must be in the text.
const (
	tierExact = iota // every word as typed
	tierForm         // every word without its particle
	tierPart         // some of the words
)

var tierNames = []string{"정확", "단어형", "부분"}

// searchUnit is one request with its answer, or one section of a record or note.
type searchUnit struct {
	doc     int
	text    string // folded text the words are looked for in
	show    string
	ref     string // where the unit came from: a request ID and line, or a heading
	heading bool   // the first line shown is the section's heading or the note's title
	tf      map[string]float64
	size    float64
}

type searchIndex struct {
	docs  []Hit
	units []searchUnit
	df    map[string]int
	avg   float64
}

// fold composes Hangul jamo, lowers case and reads _ as -, so text and query compare the same way:
// harness_recall and harness-recall are one name.
func fold(s string) string { return strings.ReplaceAll(strings.ToLower(compose(s)), "_", "-") }

// compose turns Hangul jamo (NFD, as macOS file names give them) into syllables.
func compose(s string) string {
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		l := runes[i] - 0x1100
		if l >= 0 && l < 19 && i+1 < len(runes) {
			if v := runes[i+1] - 0x1161; v >= 0 && v < 21 {
				syllable, t := 0xAC00+(l*21+v)*28, rune(0)
				i++
				if i+1 < len(runes) {
					if t = runes[i+1] - 0x11A7; t > 0 && t < 28 {
						i++
					} else {
						t = 0
					}
				}
				out = append(out, syllable+t)
				continue
			}
		}
		out = append(out, runes[i])
	}
	return string(out)
}

func hangul(r rune) bool { return r >= 0xAC00 && r <= 0xD7A3 }

func identRune(r rune) bool { return r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// keys are a folded text's index keys: overlapping bigrams of each Hangul run (a one-letter run
// stays whole), and each identifier with its camelCase, snake, kebab and path pieces.
func keys(text string, each func(string)) {
	runes := []rune(text)
	for i := 0; i < len(runes); {
		switch {
		case hangul(runes[i]):
			j := i
			for j < len(runes) && hangul(runes[j]) {
				j++
			}
			if j-i == 1 {
				each(string(runes[i]))
			}
			for k := i; k+1 < j; k++ {
				each(string(runes[k : k+2]))
			}
			i = j
		case identRune(runes[i]):
			j := i
			for j < len(runes) && (identRune(runes[j]) || (strings.ContainsRune("_-./", runes[j]) && j+1 < len(runes) && identRune(runes[j+1]))) {
				j++
			}
			identKeys(string(runes[i:j]), each)
			i = j
		default:
			i++
		}
	}
}

func identKeys(word string, each func(string)) {
	var pieces []string
	start := 0
	runes := []rune(word)
	for k := 1; k <= len(runes); k++ {
		cut := k == len(runes) || strings.ContainsRune("_-./", runes[k]) ||
			(unicode.IsUpper(runes[k]) && unicode.IsLower(runes[k-1])) ||
			(unicode.IsDigit(runes[k]) != unicode.IsDigit(runes[k-1]) && !strings.ContainsRune("_-./", runes[k-1]))
		if cut {
			if piece := strings.Trim(string(runes[start:k]), "_-./"); piece != "" {
				pieces = append(pieces, strings.ToLower(piece))
			}
			start = k
		}
	}
	whole := strings.ToLower(word)
	each(whole)
	for _, piece := range pieces {
		if piece != whole && (len(piece) >= 2 || unicode.IsDigit(rune(piece[0]))) {
			each(piece)
		}
	}
}

// newIndex cuts every document into units: a record's requests one by one, then its sections;
// a note's sections.
func newIndex(docs []Hit) *searchIndex {
	ix := &searchIndex{docs: docs, df: map[string]int{}}
	add := func(doc int, show, ref string, heading bool, parts ...string) {
		u := searchUnit{doc: doc, show: show, ref: ref, heading: heading, tf: map[string]float64{}}
		var text []string
		for i, part := range parts {
			text = append(text, fold(part))
			part = compose(part) // keys need the case to split camelCase
			weight := 1.0
			if i == 0 && len(parts) > 1 {
				weight = humanWeight
			}
			keys(part, func(k string) { u.tf[k] += weight; u.size += weight })
		}
		if u.size == 0 {
			return
		}
		u.text = strings.Join(text, "\n")
		for k := range u.tf {
			ix.df[k]++
		}
		ix.units = append(ix.units, u)
	}
	for i, d := range docs {
		for _, r := range d.requests {
			ref := ""
			if r.id != "" {
				ref = "요청 " + r.id + " 줄" + strconv.Itoa(r.line)
			}
			show := r.text
			if r.answer != "" {
				show += "\n→ AI(주장): " + r.answer
			}
			add(i, show, ref, false, r.text, r.answer)
		}
		// The title heading is the title, not a section of its own.
		title, body := d.Title, strings.Replace(d.body, "# "+d.Title+"\n", "", 1)
		if len(d.requests) > 0 {
			// A record's title is its first request, which is a unit already.
			body, title = withoutSection(body, "요청"), ""
		}
		for _, section := range sections(title, body) {
			add(i, section.text, section.heading, true, section.text)
		}
	}
	total := 0.0
	for _, u := range ix.units {
		total += u.size
	}
	if len(ix.units) > 0 {
		ix.avg = total / float64(len(ix.units))
	}
	return ix
}

type section struct{ heading, text string }

// sections splits a body at its headings; the title goes with the first one. The line shown is
// picked later, by the words searched for.
func sections(title, body string) []section {
	var out []section
	current := section{text: title}
	flush := func() {
		if strings.TrimSpace(current.text) != "" {
			out = append(out, current)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			flush()
			current = section{heading: strings.TrimSpace(strings.TrimLeft(line, "# "))}
			current.text = current.heading
			continue
		}
		current.text += "\n" + line
	}
	flush()
	return out
}

// queryWords are the words a result must hold, as typed and without their particles. Filler that
// only asks about the past does not have to be there, unless the query is nothing else.
func queryWords(query string) (typed, forms []string) {
	var words []string
	for _, field := range strings.Fields(fold(query)) {
		words = append(words, scriptWords(field)...)
	}
	for _, filler := range []bool{false, true} {
		for _, word := range words {
			word = strings.Trim(word, "?!.,~\"'`()[]")
			// PR377 and PR#377 are written "PR #377" in the records.
			if m := prWord.FindStringSubmatch(word); m != nil {
				word = "#" + m[1]
			}
			form := word
			if stripped := terms(word); len(stripped) == 1 {
				form = stripped[0]
			}
			if len([]rune(form)) < 2 || (!filler && !contentWord(form)) || slices.Contains(forms, form) {
				continue
			}
			typed, forms = append(typed, word), append(forms, form)
		}
		if len(forms) > 0 {
			break
		}
	}
	return typed, forms
}

var prWord = regexp.MustCompile(`^pr-?#?(\d+)$`)

// scriptWords splits a word where Hangul meets a Latin letter: "agent-recall레포" is a name and a
// word typed without a space between them. Digits stay with either side, as in 10월 and PR377.
func scriptWords(word string) []string {
	letter := func(r rune) bool { return r < 0x80 && unicode.IsLetter(r) }
	var out []string
	runes := []rune(word)
	start := 0
	for i := 1; i < len(runes); i++ {
		if hangul(runes[i-1]) && letter(runes[i]) || letter(runes[i-1]) && hangul(runes[i]) {
			out = append(out, string(runes[start:i]))
			start = i
		}
	}
	return append(out, string(runes[start:]))
}

// rarest keeps the n words the fewest units hold, in their order; a word no unit holds is dropped.
func (ix *searchIndex) rarest(words []string, n int) []string {
	df := map[string]int{}
	var known []string
	for _, w := range words {
		least := -1
		keys(compose(w), func(k string) {
			if d := ix.df[k]; least < 0 || d < least {
				least = d
			}
		})
		if least > 0 {
			df[w], known = least, append(known, w)
		}
	}
	if len(known) <= n {
		return known
	}
	ranked := append([]string(nil), known...)
	sort.SliceStable(ranked, func(i, j int) bool { return df[ranked[i]] < df[ranked[j]] })
	return slices.DeleteFunc(known, func(w string) bool { return !slices.Contains(ranked[:n], w) })
}

// search ranks documents by score. A unit scores BM25 over the query keys times the share of
// words it holds; the document's share of words, title and age then scale it. The tier is shown,
// not sorted on: a long conversation holds every word somewhere.
func (ix *searchIndex) search(query string, limit int, now time.Time, keep func(Hit) bool) []Hit {
	typed, forms := queryWords(query)
	if len(forms) == 0 || len(ix.units) == 0 {
		return nil
	}
	weight := map[string]float64{}
	n := float64(len(ix.units))
	for _, form := range forms {
		keys(form, func(k string) {
			if df := float64(ix.df[k]); df > 0 {
				weight[k] = math.Log(1 + (n-df+0.5)/(df+0.5))
			}
		})
	}
	// A document counts its best unit and a little of every other unit that matched. Its tier
	// comes from all its units together: keywords of one task spread over several requests.
	type best struct {
		top, sum    float64
		unit        int
		units       []int // every unit that held a word
		held, exact []bool
	}
	found := map[int]*best{}
	for i, u := range ix.units {
		if keep != nil && !keep(ix.docs[u.doc]) {
			continue
		}
		held := 0
		for _, form := range forms {
			if strings.Contains(u.text, form) {
				held++
			}
		}
		if held == 0 {
			continue
		}
		score := 0.0
		for k, w := range weight {
			if tf := u.tf[k]; tf > 0 {
				score += w * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*u.size/ix.avg))
			}
		}
		score *= float64(held) / float64(len(forms))
		b := found[u.doc]
		if b == nil {
			b = &best{held: make([]bool, len(forms)), exact: make([]bool, len(forms))}
			found[u.doc] = b
		}
		for j, form := range forms {
			if strings.Contains(u.text, form) {
				b.held[j] = true
				b.exact[j] = b.exact[j] || strings.Contains(u.text, typed[j])
			}
		}
		if score > b.top {
			b.top, b.unit = score, i
		}
		b.sum += score
		b.units = append(b.units, i)
	}
	var hits []Hit
	for doc, b := range found {
		d := ix.docs[doc]
		held, exact, titled := 0, 0, 0
		for j, form := range forms {
			if b.held[j] {
				held++
			}
			if b.exact[j] {
				exact++
			}
			if strings.Contains(fold(d.Title), form) {
				titled++
			}
		}
		tier := tierPart
		if held == len(forms) {
			tier = tierForm
			if exact == len(forms) {
				tier = tierExact
			}
		}
		cover := float64(held) / float64(len(forms))
		age := math.Max(0, now.Sub(d.mod).Hours()/24)
		d.Score = (b.top + 0.1*(b.sum-b.top)) * cover * cover * (1 + 0.4*float64(titled)/float64(len(forms))) * (0.5 + 0.5/(1+age))
		u := ix.units[b.unit]
		line, words := bestLine(u, forms, d.Title)
		ref := u.ref
		if words == 0 {
			// The best unit held the words only in the title the result names already, as a note
			// whose title stands alone before its first heading; another unit shows where they are.
			for _, i := range b.units {
				if l, n := bestLine(ix.units[i], forms, d.Title); n > words {
					line, words, ref = l, n, ix.units[i].ref
				}
			}
		}
		d.Tier, d.Ref, d.Line = tierNames[tier], ref, line
		hits = append(hits, d)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].mod.After(hits[j].mod)
	})
	return hits[:min(limit, len(hits))]
}

// bestLine is the unit's line holding the most words, the request itself before its answer. A
// section's heading counts last, so a line under it with as many words wins. The result names the
// title already, so only what a line says beyond it is shown; when no line holds a word, the first
// one with anything left is, or none. The count is how many words the line shown holds.
func bestLine(u searchUnit, forms []string, title string) (string, int) {
	lines := strings.Split(u.show, "\n")
	if u.heading {
		lines = append(lines[1:], lines[0])
	}
	shown, best := "", -1
	for _, line := range lines {
		if line = beyondTitle(line, title); line == "" {
			continue
		}
		count := 0
		for _, form := range forms {
			if strings.Contains(fold(line), form) {
				count++
			}
		}
		if count > best {
			best, shown = count, line
		}
	}
	return shown, max(best, 0)
}

// beyondTitle is what a line says beyond the title: nothing when it is the title, and the rest
// when it opens with a title cut short with "…", as a record's first request does.
func beyondTitle(line, title string) string {
	flat, title := strings.Join(strings.Fields(line), " "), strings.Join(strings.Fields(title), " ")
	if prefix, cut := strings.CutSuffix(title, "…"); cut && prefix != "" && strings.HasPrefix(flat, prefix) {
		if rest := strings.TrimSpace(flat[len(prefix):]); rest != "" {
			return "…" + rest
		}
		return ""
	}
	if flat == title {
		return ""
	}
	return strings.TrimSpace(line)
}
