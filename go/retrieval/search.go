// Package retrieval implements the shared lexical lookup policy used by the CLI,
// concept evaluator, and benchmark. It leaves the corpus's indexed content unchanged.
package retrieval

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type Strategy string

const (
	StrategyExact    Strategy = "exact"
	StrategySafeFill Strategy = "safe-fill"
	StrategyFused    Strategy = "fused"

	// Relax only genuinely sparse result sets. This keeps the normal one-query path
	// for broad concept queries while still recovering from vocabulary mismatches.
	relaxationThreshold = 7
	rrfK                = 60.0
	exactRRFWeight      = 4.0
	relaxedRRFWeight    = 1.0
	orRRFWeight         = 0.5
)

type Hit struct {
	SourceRel string
	Section   string
	PageID    string
	Title     string
	PageKey   string
}

type Stats struct {
	QueryCount       int
	VocabularyLookup int
	Relaxed          bool
	UsedORFallback   bool
}

type Searcher struct {
	db *sql.DB
}

func New(db *sql.DB) *Searcher {
	return &Searcher{db: db}
}

func ParseStrategy(value string) (Strategy, error) {
	switch Strategy(value) {
	case StrategyExact, StrategySafeFill, StrategyFused:
		return Strategy(value), nil
	default:
		return "", fmt.Errorf("unsupported retrieval policy %q (want exact, safe-fill, or fused)", value)
	}
}

// Terms turns a natural-language query into FTS5 literals. In particular, it makes AND,
// OR, NOT, and punctuation ordinary query terms instead of allowing them to alter FTS syntax.
func Terms(query string) []string {
	var terms []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 1 {
			terms = append(terms, word.String())
		}
		word.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func (s *Searcher) Search(query string, limit int, strategy Strategy) ([]Hit, Stats, error) {
	return s.SearchTerms(Terms(query), limit, strategy)
}

func (s *Searcher) SearchTerms(terms []string, limit int, strategy Strategy) ([]Hit, Stats, error) {
	if _, err := ParseStrategy(string(strategy)); err != nil {
		return nil, Stats{}, err
	}
	if len(terms) == 0 {
		return nil, Stats{}, fmt.Errorf("query has no FTS terms")
	}
	if limit < 1 {
		limit = 10
	}

	stats := Stats{}
	exact, err := s.query(matchAll(terms), limit, &stats)
	if err != nil {
		return nil, stats, err
	}
	trigger := relaxationThreshold
	if limit < trigger {
		trigger = limit
	}
	if strategy == StrategyExact || len(exact) >= trigger || len(terms) < 2 {
		return exact, stats, nil
	}

	drop, err := s.leastDiscriminativeTerm(terms, &stats)
	if err != nil {
		return nil, stats, err
	}
	relaxedTerms := append([]string{}, terms[:drop]...)
	relaxedTerms = append(relaxedTerms, terms[drop+1:]...)
	relaxed, err := s.query(matchAll(relaxedTerms), limit, &stats)
	if err != nil {
		return nil, stats, err
	}
	stats.Relaxed = true

	var orHits []Hit
	if len(exact) == 0 && len(relaxed) < limit {
		orHits, err = s.query(matchAny(terms), limit, &stats)
		if err != nil {
			return nil, stats, err
		}
		stats.UsedORFallback = true
	}

	if strategy == StrategySafeFill {
		return appendUnique(limit, exact, relaxed, orHits), stats, nil
	}
	return fuse(limit, rankedList{hits: exact, weight: exactRRFWeight}, rankedList{hits: relaxed, weight: relaxedRRFWeight}, rankedList{hits: orHits, weight: orRRFWeight}), stats, nil
}

func (s *Searcher) leastDiscriminativeTerm(terms []string, stats *Stats) (int, error) {
	index, largest := 0, -1
	for i, term := range terms {
		var frequency int
		err := s.db.QueryRow("SELECT doc FROM pages_fts_vocab WHERE term = ?", term).Scan(&frequency)
		if err != nil && strings.Contains(err.Error(), "no such table: pages_fts_vocab") {
			// Corpora built before the metadata table was added stay searchable. Rebuilding them
			// upgrades this compatibility path to fts5vocab without changing indexed content.
			err = s.db.QueryRow("SELECT count(*) FROM pages_fts WHERE pages_fts MATCH ?", matchAll([]string{term})).Scan(&frequency)
		}
		if err == sql.ErrNoRows {
			frequency = 0
		} else if err != nil {
			return 0, err
		}
		stats.VocabularyLookup++
		// Earlier query terms win ties, keeping the relaxation deterministic.
		if frequency > largest {
			index, largest = i, frequency
		}
	}
	return index, nil
}

func (s *Searcher) query(expression string, limit int, stats *Stats) ([]Hit, error) {
	rows, err := s.db.Query(`SELECT p.source_rel, p.section, p.page_id, p.title, p.page_key
FROM pages_fts f JOIN pages p ON p.rowid = f.rowid
WHERE pages_fts MATCH ?
ORDER BY bm25(pages_fts, 0.0, 10.0, 1.0) LIMIT ?`, expression, limit)
	stats.QueryCount++
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var hit Hit
		if err := rows.Scan(&hit.SourceRel, &hit.Section, &hit.PageID, &hit.Title, &hit.PageKey); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func matchAll(terms []string) string {
	return strings.Join(quoteTerms(terms), " ")
}

func matchAny(terms []string) string {
	return strings.Join(quoteTerms(terms), " OR ")
}

func quoteTerms(terms []string) []string {
	quoted := make([]string, len(terms))
	for i, term := range terms {
		quoted[i] = `"` + term + `"`
	}
	return quoted
}

func appendUnique(limit int, lists ...[]Hit) []Hit {
	seen := map[string]bool{}
	result := make([]Hit, 0, limit)
	for _, hits := range lists {
		for _, hit := range hits {
			if seen[hit.SourceRel] {
				continue
			}
			seen[hit.SourceRel] = true
			result = append(result, hit)
			if len(result) == limit {
				return result
			}
		}
	}
	return result
}

type rankedList struct {
	hits   []Hit
	weight float64
}

func fuse(limit int, lists ...rankedList) []Hit {
	type candidate struct {
		hit   Hit
		score float64
	}
	candidates := map[string]candidate{}
	for _, list := range lists {
		for rank, hit := range list.hits {
			candidate := candidates[hit.SourceRel]
			candidate.hit = hit
			candidate.score += list.weight / (rrfK + float64(rank+1))
			candidates[hit.SourceRel] = candidate
		}
	}
	ordered := make([]candidate, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		return ordered[i].hit.SourceRel < ordered[j].hit.SourceRel
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	result := make([]Hit, len(ordered))
	for i, candidate := range ordered {
		result[i] = candidate.hit
	}
	return result
}
